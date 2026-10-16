// Package sitetest is an offline conformance suite for site adapters, in the
// spirit of testing/fstest. Every built-in adapter must pass TestAdapter.
//
// The suite never performs network I/O. It only exercises descriptor and
// config surfaces, plus session paths that must fail before the first
// request. It must not import internal/site/sites, so adapter packages can
// call it from their own tests without an import cycle.
package sitetest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// Samples are synthetic, adapter-specific inputs the suite needs to get past
// argument validation. They must never be real cookies, tokens, or torrent
// IDs tied to an account.
type Samples struct {
	// Credential uses a declared auth method and passes the adapter's own
	// credential format checks.
	Credential site.Credential
	// RemoteID is a canonical torrent ID for the detail and metafile ports.
	RemoteID string
	// BonusSelector is accepted by the bonus review and exchange ports.
	BonusSelector string
}

// TestAdapter checks that adapter's descriptor, typed ports, configs, and
// pre-request session behavior agree with each other.
func TestAdapter(t *testing.T, adapter site.Adapter, samples Samples) {
	t.Helper()
	if adapter == nil {
		t.Fatal("sitetest: nil adapter")
	}
	descriptor := adapter.Descriptor()
	t.Run("descriptor", func(t *testing.T) { testDescriptor(t, adapter) })
	t.Run("capabilities", func(t *testing.T) {
		for _, capability := range descriptor.Capabilities {
			if !site.ImplementsCapability(adapter, capability) {
				t.Errorf("declares %q without implementing its port", capability)
			}
		}
	})
	t.Run("samples", func(t *testing.T) {
		if !descriptor.SupportsAuth(samples.Credential.Method()) {
			t.Fatalf("sample credential method %q is not declared", samples.Credential.Method())
		}
	})
	for _, port := range directPorts(adapter) {
		if !descriptor.Supports(port.capability) {
			continue
		}
		t.Run(string(port.capability), func(t *testing.T) {
			if err := port.call(canceledContext(), samples.Credential); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled context err=%v, want context.Canceled before any request", err)
			}
		})
	}
	if lister, ok := adapter.(site.AccountTorrentLister); ok && descriptor.Supports(domain.CapabilityAccountTorrents) {
		t.Run(string(domain.CapabilityAccountTorrents)+"/query", func(t *testing.T) { testAccountTorrentQuery(t, lister) })
	}
	if loginer, ok := adapter.(site.SessionLoginer); ok && descriptor.Supports(domain.CapabilityLogin) {
		t.Run(string(domain.CapabilityLogin), func(t *testing.T) { testLogin(t, loginer) })
	}
	for _, port := range configuredPorts(adapter, descriptor, samples) {
		t.Run(string(port.capability), func(t *testing.T) {
			if descriptor.Supports(port.capability) {
				testDeclaredPort(t, descriptor, port, samples)
			} else {
				testUndeclaredPort(t, port, samples)
			}
		})
	}
}

// testAccountTorrentQuery checks that every lister serves the seeding list
// and refuses an unknown state or a negative page.
func testAccountTorrentQuery(t *testing.T, lister site.AccountTorrentLister) {
	if err := lister.ValidateAccountTorrentQuery(site.AccountTorrentQuery{State: domain.AccountTorrentsSeeding}); err != nil {
		t.Fatalf("seeding query rejected: %v", err)
	}
	for _, query := range []site.AccountTorrentQuery{
		{},
		{State: "sitetest-unknown"},
		{State: domain.AccountTorrentsSeeding, Page: -1},
	} {
		if err := lister.ValidateAccountTorrentQuery(query); err == nil {
			t.Fatalf("query %+v accepted", query)
		}
	}
}

// testLogin checks the login config and that a done context stops a login
// before its first request. The credentials are synthetic.
func testLogin(t *testing.T, loginer site.SessionLoginer) {
	config := loginer.LoginConfig()
	seen := map[int]bool{}
	for _, days := range config.SessionDays {
		if days <= 0 || seen[days] {
			t.Fatalf("login session days %v must be positive and unique", config.SessionDays)
		}
		seen[days] = true
	}
	if !config.Allows(config.DefaultSessionDays) {
		t.Fatalf("default login session days %d is not offered in %v", config.DefaultSessionDays, config.SessionDays)
	}
	config.SessionDays[0] = -1
	if loginer.LoginConfig().Allows(-1) {
		t.Fatal("LoginConfig shares its session days with callers")
	}
	password, err := site.NewAccountPassword("sitetest-PASSWORD-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := site.NewLoginCredentials("sitetest-user", password, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := loginer.Login(canceledContext(), credentials, loginer.LoginConfig().DefaultSessionDays)
	if !errors.Is(err, context.Canceled) || result.Session != (site.Credential{}) || result.RequestsAttempted != 0 {
		t.Fatalf("canceled login err=%v requests=%d, want context.Canceled before any request", err, result.RequestsAttempted)
	}
}

func testDescriptor(t *testing.T, adapter site.Adapter) {
	first := cloneDescriptor(adapter.Descriptor())
	if err := site.ValidateDescriptor(first); err != nil {
		t.Fatal(err)
	}
	mutated := adapter.Descriptor()
	if !reflect.DeepEqual(first, mutated) {
		t.Fatalf("descriptor is not stable across calls:\n%+v\n%+v", first, mutated)
	}
	if len(mutated.AuthMethods) > 0 {
		mutated.AuthMethods[0] = "sitetest.mutated"
	}
	if len(mutated.Capabilities) > 0 {
		mutated.Capabilities[0] = "sitetest.mutated"
	}
	if again := adapter.Descriptor(); !reflect.DeepEqual(first, again) {
		t.Fatalf("descriptor shares mutable slices with callers:\n%+v\n%+v", first, again)
	}
}

func cloneDescriptor(descriptor domain.SiteDescriptor) domain.SiteDescriptor {
	descriptor.AuthMethods = append([]domain.AuthMethod(nil), descriptor.AuthMethods...)
	descriptor.Capabilities = append([]domain.Capability(nil), descriptor.Capabilities...)
	return descriptor
}

// directPort contacts the site on every call with no separate session, so the
// suite can only check that a done context stops it before any request.
type directPort struct {
	capability domain.Capability
	call       func(context.Context, site.Credential) error
}

func directPorts(adapter site.Adapter) []directPort {
	var ports []directPort
	if port, ok := adapter.(site.AuthChecker); ok {
		ports = append(ports, directPort{domain.CapabilityAuthCheck, func(ctx context.Context, credential site.Credential) error {
			_, err := port.CheckSession(ctx, credential)
			return err
		}})
	}
	if port, ok := adapter.(site.AccountReader); ok {
		ports = append(ports, directPort{domain.CapabilityAccountRead, func(ctx context.Context, credential site.Credential) error {
			_, err := port.Account(ctx, credential)
			return err
		}})
	}
	if port, ok := adapter.(site.TorrentSearcher); ok {
		ports = append(ports, directPort{domain.CapabilitySearch, func(ctx context.Context, credential site.Credential) error {
			_, err := port.Search(ctx, credential, "sitetest")
			return err
		}})
	}
	if port, ok := adapter.(site.BonusCatalogReader); ok {
		ports = append(ports, directPort{domain.CapabilityBonusRead, func(ctx context.Context, credential site.Credential) error {
			_, err := port.BonusCatalog(ctx, credential)
			return err
		}})
	}
	if port, ok := adapter.(site.AccountTorrentLister); ok {
		ports = append(ports, directPort{domain.CapabilityAccountTorrents, func(ctx context.Context, credential site.Credential) error {
			_, err := port.AccountTorrents(ctx, credential, site.AccountTorrentQuery{State: domain.AccountTorrentsSeeding})
			return err
		}})
	}
	return ports
}

// configuredPort adapts one config-bearing port to a uniform shape. Each such
// port pins a credential-free config, validates input before any credential is
// read, and opens a session whose failure paths must not reach the network.
type configuredPort struct {
	capability domain.Capability
	config     func() (origin string, routeIDs []string, err error)
	// accept validates the sample input; reject validates each malformed one.
	accept func() error
	reject []func() error
	open   func(context.Context, site.Credential) (*portSession, error)
	// refusedOpen, when set, is a second way to open the session that must
	// always fail, such as a cookie-only open on a bonus exchange writer that
	// needs the account password.
	refusedOpen func(context.Context, site.Credential) error
	// invalidStop is the receipt stop reason for an input the session rejects.
	invalidStop string
	// closedStop is the receipt stop reason after Close, or "" when the port
	// only promises an error.
	closedStop string
}

type portSession struct {
	// read uses the sample input when valid is true, otherwise an input the
	// session must reject.
	read     func(ctx context.Context, valid bool) readResult
	requests func() int
	close    func() error
}

type readResult struct {
	stop, origin, routeID string
	err                   error
}

func configuredPorts(adapter site.Adapter, descriptor domain.SiteDescriptor, samples Samples) []configuredPort {
	validRef := domain.TorrentRef{SiteID: descriptor.ID, RemoteID: samples.RemoteID}
	invalidRefs := []domain.TorrentRef{
		{SiteID: descriptor.ID + "-foreign", RemoteID: samples.RemoteID},
		{SiteID: strings.ToUpper(descriptor.ID), RemoteID: samples.RemoteID},
		{SiteID: "", RemoteID: samples.RemoteID},
		{SiteID: descriptor.ID, RemoteID: ""},
		{SiteID: descriptor.ID, RemoteID: " " + samples.RemoteID},
		{SiteID: descriptor.ID, RemoteID: samples.RemoteID + " "},
		{SiteID: descriptor.ID, RemoteID: samples.RemoteID + "/x"},
	}
	invalidSelectors := []string{"", " " + samples.BonusSelector, samples.BonusSelector + " ", samples.BonusSelector + "\n"}
	ref := func(valid bool) domain.TorrentRef {
		if valid {
			return validRef
		}
		return invalidRefs[0]
	}
	selector := func(valid bool) string {
		if valid {
			return samples.BonusSelector
		}
		return ""
	}
	rejectRefs := func(validate func(domain.TorrentRef) error) []func() error {
		checks := make([]func() error, len(invalidRefs))
		for i, invalid := range invalidRefs {
			checks[i] = func() error { return validate(invalid) }
		}
		return checks
	}
	rejectSelectors := func(validate func(string) error) []func() error {
		checks := make([]func() error, len(invalidSelectors))
		for i, invalid := range invalidSelectors {
			checks[i] = func() error { return validate(invalid) }
		}
		return checks
	}

	var ports []configuredPort
	if port, ok := adapter.(site.TorrentDetailReader); ok {
		ports = append(ports, configuredPort{
			capability: domain.CapabilityDetail,
			config: func() (string, []string, error) {
				config, err := port.TorrentDetailConfig()
				return config.Origin, []string{config.RouteID}, err
			},
			accept: func() error { return port.ValidateTorrentDetailRef(validRef) },
			reject: rejectRefs(port.ValidateTorrentDetailRef),
			open: func(ctx context.Context, credential site.Credential) (*portSession, error) {
				session, err := port.OpenTorrentDetailSession(ctx, credential)
				if err != nil || session == nil {
					return nil, sessionOpenError(err)
				}
				return &portSession{
					read: func(ctx context.Context, valid bool) readResult {
						_, receipt, err := session.ReadTorrentDetail(ctx, ref(valid), site.DefaultTorrentDetailLimits())
						return readResult{receipt.StopReason, receipt.Origin, receipt.RouteID, err}
					},
					requests: session.RequestsMade,
					close:    session.Close,
				}, nil
			},
			invalidStop: "invalid_reference",
			closedStop:  "session_closed",
		})
	}
	if port, ok := adapter.(site.MetafileFetcher); ok {
		ports = append(ports, configuredPort{
			capability: domain.CapabilityMetafile,
			config: func() (string, []string, error) {
				config, err := port.MetafileFetchConfig()
				return config.Origin, []string{config.RouteID}, err
			},
			accept: func() error { return port.ValidateMetafileRef(validRef) },
			reject: rejectRefs(port.ValidateMetafileRef),
			open: func(ctx context.Context, credential site.Credential) (*portSession, error) {
				session, err := port.OpenMetafileFetchSession(ctx, credential)
				if err != nil || session == nil {
					return nil, sessionOpenError(err)
				}
				return &portSession{
					read: func(ctx context.Context, valid bool) readResult {
						config, err := port.MetafileFetchConfig()
						if err != nil {
							return readResult{err: err}
						}
						_, receipt, err := session.FetchMetafile(ctx, ref(valid), config.DefaultLimits())
						return readResult{receipt.StopReason, receipt.Origin, receipt.RouteID, err}
					},
					requests: session.RequestsMade,
					close:    session.Close,
				}, nil
			},
			invalidStop: "invalid_reference",
			closedStop:  "session_closed",
		})
	}
	if port, ok := adapter.(site.BonusReviewReader); ok {
		ports = append(ports, configuredPort{
			capability: domain.CapabilityBonusReview,
			config: func() (string, []string, error) {
				config, err := port.BonusReviewConfig()
				return config.Origin, []string{config.RouteID}, err
			},
			accept: func() error { return port.ValidateBonusReviewSelector(samples.BonusSelector) },
			reject: rejectSelectors(port.ValidateBonusReviewSelector),
			open: func(ctx context.Context, credential site.Credential) (*portSession, error) {
				session, err := port.OpenBonusReviewSession(ctx, credential)
				if err != nil || session == nil {
					return nil, sessionOpenError(err)
				}
				return &portSession{
					read: func(ctx context.Context, valid bool) readResult {
						_, receipt, err := session.ReadBonusReview(ctx, selector(valid), site.DefaultBonusReviewLimits())
						return readResult{receipt.StopReason, receipt.Origin, receipt.RouteID, err}
					},
					requests: session.RequestsMade,
					close:    session.Close,
				}, nil
			},
			invalidStop: "invalid_selector",
			closedStop:  "session_closed",
		})
	}
	if port, ok := adapter.(site.BonusExchangeWriter); ok {
		openExchange := port.OpenBonusExchangeSession
		var refusedOpen func(context.Context, site.Credential) error
		if passwordPort, ok := port.(site.PasswordConfirmedBonusExchangeWriter); ok {
			// The suite's password is synthetic; no session path that must fail
			// before the first request ever sends it.
			password, _ := site.NewAccountPassword("sitetest-PASSWORD-CANARY")
			openExchange = func(ctx context.Context, credential site.Credential) (site.BonusExchangeSession, error) {
				return passwordPort.OpenPasswordConfirmedBonusExchangeSession(ctx, credential, password)
			}
			refusedOpen = func(ctx context.Context, credential site.Credential) error {
				session, err := port.OpenBonusExchangeSession(ctx, credential)
				if session != nil {
					_ = session.Close()
				}
				return err
			}
		}
		ports = append(ports, configuredPort{
			capability: domain.CapabilityBonusExchange,
			config: func() (string, []string, error) {
				config, err := port.BonusExchangeConfig()
				return config.Origin, []string{config.ReviewRouteID, config.ActionRouteID}, err
			},
			accept:      func() error { return port.ValidateBonusExchangeSelector(samples.BonusSelector) },
			reject:      rejectSelectors(port.ValidateBonusExchangeSelector),
			refusedOpen: refusedOpen,
			open: func(ctx context.Context, credential site.Credential) (*portSession, error) {
				session, err := openExchange(ctx, credential)
				if err != nil || session == nil {
					return nil, sessionOpenError(err)
				}
				return &portSession{
					read: func(ctx context.Context, valid bool) readResult {
						_, receipt, err := session.ReadFreshBonusReview(ctx, selector(valid), site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
						return readResult{receipt.StopReason, receipt.Origin, receipt.RouteID, err}
					},
					requests: session.RequestsMade,
					close:    session.Close,
				}, nil
			},
			invalidStop: "invalid_selector",
		})
	}
	return ports
}

func sessionOpenError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("sitetest: adapter returned a nil session without an error")
}

func testDeclaredPort(t *testing.T, descriptor domain.SiteDescriptor, port configuredPort, samples Samples) {
	origin, routeIDs, err := port.config()
	if err != nil {
		t.Fatalf("declared port config failed: %v", err)
	}
	descriptorOrigin, err := site.DescriptorOrigin(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if origin != descriptorOrigin || site.ValidateOrigin(origin) != nil {
		t.Fatalf("config origin %q does not match descriptor origin %q", origin, descriptorOrigin)
	}
	for _, routeID := range routeIDs {
		if !strings.HasPrefix(routeID, descriptor.ID+".") || site.ValidateRouteID(routeID) != nil {
			t.Fatalf("route ID %q must be valid and start with %q", routeID, descriptor.ID+".")
		}
	}
	if err := port.accept(); err != nil {
		t.Fatalf("sample input rejected: %v", err)
	}
	for i, reject := range port.reject {
		if reject() == nil {
			t.Fatalf("malformed input #%d accepted", i)
		}
	}

	if session, err := port.open(canceledContext(), samples.Credential); err == nil {
		session.close()
		t.Fatal("opened a session with a canceled context")
	}
	undeclared, err := site.NewCredential("sitetest_undeclared", "sitetest-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	if session, err := port.open(context.Background(), undeclared); err == nil {
		session.close()
		t.Fatal("opened a session with an undeclared auth method")
	}
	if port.refusedOpen != nil && port.refusedOpen(context.Background(), samples.Credential) == nil {
		t.Fatal("opened a session through a path the port must refuse")
	}

	openSession := func(t *testing.T) *portSession {
		t.Helper()
		session, err := port.open(context.Background(), samples.Credential)
		if err != nil {
			t.Fatalf("open with sample credential failed: %v", err)
		}
		t.Cleanup(func() { session.close() })
		return session
	}
	checkRead := func(t *testing.T, session *portSession, result readResult, wantStop string) {
		t.Helper()
		if result.err == nil {
			t.Fatal("read succeeded")
		}
		if wantStop != "" && result.stop != wantStop {
			t.Fatalf("stop=%q want %q (err=%v)", result.stop, wantStop, result.err)
		}
		if made := session.requests(); made != 0 {
			t.Fatalf("made %d requests before failing", made)
		}
		if result.stop != "" && (result.origin != origin || !contains(routeIDs, result.routeID)) {
			t.Fatalf("receipt origin/route %q %q do not match the pinned config", result.origin, result.routeID)
		}
	}
	t.Run("invalid input", func(t *testing.T) {
		session := openSession(t)
		checkRead(t, session, session.read(context.Background(), false), port.invalidStop)
	})
	t.Run("canceled context", func(t *testing.T) {
		session := openSession(t)
		result := session.read(canceledContext(), true)
		checkRead(t, session, result, "context_done")
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("err=%v does not wrap context.Canceled", result.err)
		}
	})
	t.Run("closed session", func(t *testing.T) {
		session := openSession(t)
		if err := session.close(); err != nil {
			t.Fatalf("close failed: %v", err)
		}
		checkRead(t, session, session.read(context.Background(), true), port.closedStop)
	})
}

func testUndeclaredPort(t *testing.T, port configuredPort, samples Samples) {
	if _, _, err := port.config(); err == nil {
		t.Fatal("undeclared port returned a config")
	}
	if port.accept() == nil {
		t.Fatal("undeclared port accepted the sample input")
	}
	if session, err := port.open(context.Background(), samples.Credential); err == nil {
		session.close()
		t.Fatal("undeclared port opened a session")
	}
	if port.refusedOpen != nil && port.refusedOpen(context.Background(), samples.Credential) == nil {
		t.Fatal("undeclared port opened a session through a refused path")
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
