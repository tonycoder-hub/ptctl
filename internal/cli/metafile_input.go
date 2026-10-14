package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
)

func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == name {
			found = true
		}
	})
	return found
}

type metafileInput struct {
	path      string
	storeRoot string
	variantID metastore.ArtifactID
}

func positionalMetafileInput(command string, args []string, storeRoot, variantID string, storeSet, variantSet bool) (metafileInput, error) {
	if storeSet || variantSet {
		if storeRoot == "" || variantID == "" {
			return metafileInput{}, usageError("%s requires --metafile-store and --metafile-variant together", command)
		}
		if len(args) != 0 {
			return metafileInput{}, usageError("%s accepts either one FILE.torrent or the stored-artifact flags, not both", command)
		}
		id, err := metastore.ParseArtifactID(variantID)
		if err != nil {
			return metafileInput{}, usageError("%s requires a canonical sha256 metafile variant ID", command)
		}
		return metafileInput{storeRoot: storeRoot, variantID: id}, nil
	}
	if len(args) != 1 {
		return metafileInput{}, usageError("%s requires one FILE.torrent or --metafile-store with --metafile-variant", command)
	}
	return metafileInput{path: args[0]}, nil
}

func flaggedMetafileInput(command, path, storeRoot, variantID string, pathSet, storeSet, variantSet bool) (metafileInput, error) {
	stored := storeSet || variantSet
	file := pathSet || path != ""
	if stored {
		if storeRoot == "" || variantID == "" {
			return metafileInput{}, usageError("%s requires --metafile-store and --metafile-variant together", command)
		}
		if file {
			return metafileInput{}, usageError("%s accepts either --torrent or the stored-artifact flags, not both", command)
		}
		id, err := metastore.ParseArtifactID(variantID)
		if err != nil {
			return metafileInput{}, usageError("%s requires a canonical sha256 metafile variant ID", command)
		}
		return metafileInput{storeRoot: storeRoot, variantID: id}, nil
	}
	if !file || path == "" {
		return metafileInput{}, usageError("%s requires --torrent or --metafile-store with --metafile-variant", command)
	}
	return metafileInput{path: path}, nil
}

func loadMetafileInput(ctx context.Context, input metafileInput) (*metafile.MetaInfo, error) {
	if input.path != "" {
		return metafile.Read(input.path)
	}
	store, err := metastore.Open(input.storeRoot)
	if err != nil {
		return nil, err
	}
	meta, _, err := store.Load(ctx, input.variantID, metastore.DefaultLimits())
	if errors.Is(err, metastore.ErrCorruptArtifact) {
		return nil, storedMetafileIntegrityErr("stored metafile", err)
	}
	return meta, err
}

// storedMetafileIntegrityErr maps a corrupt stored artifact to an integrity
// failure. One that still matches its digest but no longer parses was stored
// under a less strict parser; the same bytes are rejected again, so the
// remedy is a canonical copy under a new variant ID.
func storedMetafileIntegrityErr(subject string, err error) error {
	if errors.Is(err, metastore.ErrArtifactUnparseable) {
		return &integrityErr{message: fmt.Sprintf("%s: %v; re-fetch it with ptctl site metafile fetch or import a canonical copy with ptctl metafile store import (the same bytes are rejected again), then use the new variant ID", subject, err)}
	}
	return &integrityErr{message: subject + " failed digest or parse validation"}
}
