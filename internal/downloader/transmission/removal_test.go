package transmission

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

func TestExistingJobRemovalUsesExactKeepDataRequestForBothProtocols(t *testing.T) {
	for _, modern := range []bool{false, true} {
		name := "legacy"
		if modern {
			name = "json-rpc"
		}
		t.Run(name, func(t *testing.T) {
			server, state := newControlFixture(t, modern, "success")
			defer server.Close()
			adapter, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			credential, _ := downloader.NewCredential("alice", "secret")
			session, err := adapter.OpenExistingJobRemovalSession(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if session.RequestsMade() != 2 {
				t.Fatalf("open requests = %d", session.RequestsMade())
			}
			descriptor, err := session.ReadExistingJobRemovalDescriptor(context.Background())
			if err != nil || session.RequestsMade() != 2 {
				t.Fatalf("descriptor=%#v requests=%d err=%v", descriptor, session.RequestsMade(), err)
			}
			receipt, err := session.RemoveKeepData(context.Background(), downloader.ExistingJobMutationRequest{JobKey: testHash})
			captured, wireRequests, actionRequests := state.snapshot()
			if err != nil || !receipt.Complete || receipt.Effect != downloader.RemoveEffectKeepData || receipt.RequestsAttempted != 1 ||
				receipt.AutomaticRetries != 0 || receipt.RedirectsFollowed != 0 || !receipt.RequestBytesKnown ||
				receipt.RequestBytes != int64(len(captured)) || receipt.RequestID <= 0 ||
				session.RequestsMade() != 3 || wireRequests != 3 || actionRequests != 1 {
				t.Fatalf("receipt=%#v session=%d wire=%d actions=%d err=%v", receipt, session.RequestsMade(), wireRequests, actionRequests, err)
			}
			expected, err := downloader.MarshalExistingJobRemovalRequest(descriptor, testHash, receipt.RequestID)
			if err != nil || !bytes.Equal(captured, expected) {
				t.Fatalf("wire request differs:\n got %s\nwant %s\nerr=%v", captured, expected, err)
			}
			second, secondErr := session.RemoveKeepData(context.Background(), downloader.ExistingJobMutationRequest{JobKey: testHash})
			if secondErr == nil || second.RequestsAttempted != 0 || session.RequestsMade() != 3 {
				t.Fatalf("second=%#v requests=%d err=%v", second, session.RequestsMade(), secondErr)
			}
		})
	}
}

func TestExistingJobRemovalNeverReplaysExpiredCSRFOrLeaksSelector(t *testing.T) {
	server, state := newControlFixture(t, true, "csrf")
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenExistingJobRemovalSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ReadExistingJobRemovalDescriptor(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.RemoveKeepData(context.Background(), downloader.ExistingJobMutationRequest{JobKey: testHash})
	captured, requests, actions := state.snapshot()
	if err == nil || receipt.Complete || receipt.StopReason != "csrf_expired" || receipt.RequestsAttempted != 1 ||
		!receipt.RequestBytesKnown || receipt.RequestBytes != int64(len(captured)) || receipt.RequestID <= 0 ||
		session.RequestsMade() != 3 || requests != 3 || actions != 1 || strings.Contains(err.Error(), testHash) || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("receipt=%#v requests=%d actions=%d err=%v", receipt, requests, actions, err)
	}
}
