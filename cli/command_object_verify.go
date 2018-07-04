package cli

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/kopia/kopia/block"
	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/parallelwork"
	"github.com/kopia/kopia/object"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
)

var (
	verifyCommand               = objectCommands.Command("verify", "Verify the contents of stored object")
	verifyCommandErrorThreshold = verifyCommand.Flag("max-errors", "Maximum number of errors before stopping").Default("0").Int()
	verifyCommandDirObjectIDs   = verifyCommand.Flag("directory-id", "Directory object IDs to verify").Strings()
	verifyCommandFileObjectIDs  = verifyCommand.Flag("file-id", "File object IDs to verify").Strings()
	verifyCommandAllSources     = verifyCommand.Flag("all-sources", "Verify all snapshots").Bool()
	verifyCommandSources        = verifyCommand.Flag("sources", "Verify the provided sources").Strings()
	verifyCommandParallel       = verifyCommand.Flag("parallel", "Parallelization").Default("16").Int()
	verifyCommandFilesPercent   = verifyCommand.Flag("verify-files-percent", "Randomly verify a percentage of files").Default("0").Int()
)

type verifier struct {
	mgr       *snapshot.Manager
	om        *object.Manager
	workQueue *parallelwork.Queue
	startTime time.Time

	mu   sync.Mutex
	seen map[object.ID]bool

	errors []error
}

func (v *verifier) progressCallback(enqueued, active, completed int64) {
	elapsed := time.Since(v.startTime)
	maybeTimeRemaining := ""
	if elapsed > 1*time.Second && enqueued > 0 && completed > 0 {
		completedRatio := float64(completed) / float64(enqueued)
		predictedSeconds := elapsed.Seconds() / completedRatio
		predictedEndTime := v.startTime.Add(time.Duration(predictedSeconds) * time.Second)

		dt := time.Until(predictedEndTime)
		if dt > 0 {
			maybeTimeRemaining = fmt.Sprintf(" remaining %v (ETA %v)", dt.Truncate(1*time.Second), predictedEndTime.Truncate(1*time.Second).Format(timeFormat))
		}
	}
	fmt.Fprintf(os.Stderr, "Found %v objects, verifying %v, completed %v objects%v.\n", enqueued, active, completed, maybeTimeRemaining)
}

func (v *verifier) tooManyErrors() bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	if *verifyCommandErrorThreshold == 0 {
		return false
	}

	return len(v.errors) >= *verifyCommandErrorThreshold
}

func (v *verifier) reportError(path string, err error) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	log.Warningf("failed on %v: %v", path, err)
	v.errors = append(v.errors, err)
	return len(v.errors) >= *verifyCommandErrorThreshold
}

func (v *verifier) shouldEnqueue(oid object.ID) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.seen[oid] {
		return false
	}

	v.seen[oid] = true
	return true
}

func (v *verifier) enqueueVerifyDirectory(ctx context.Context, oid object.ID, path string) {
	// push to the front of the queue, so that we quickly discover all directories to get reliable ETA.
	if !v.shouldEnqueue(oid) {
		return
	}
	v.workQueue.EnqueueFront(func() {
		v.doVerifyDirectory(ctx, oid, path)
	})
}

func (v *verifier) enqueueVerifyObject(ctx context.Context, oid object.ID, path string, expectedLength int64) {
	// push to the back of the queue, so that we process non-directories at the end.
	if !v.shouldEnqueue(oid) {
		return
	}
	v.workQueue.EnqueueBack(func() {
		v.doVerifyObject(ctx, oid, path, expectedLength)
	})
}

func (v *verifier) doVerifyDirectory(ctx context.Context, oid object.ID, path string) {
	log.Debugf("verifying directory %q (%v)", path, oid)

	d := v.mgr.DirectoryEntry(oid, nil)
	entries, err := d.Readdir(ctx)
	if err != nil {
		v.reportError(path, fmt.Errorf("error reading %v: %v", oid, err))
		return
	}

	for _, e := range entries {
		if v.tooManyErrors() {
			break
		}

		m := e.Metadata()
		objectID := e.(object.HasObjectID).ObjectID()
		childPath := path + "/" + m.Name
		if m.FileMode().IsDir() {
			v.enqueueVerifyDirectory(ctx, objectID, childPath)
		} else {
			v.enqueueVerifyObject(ctx, objectID, childPath, m.FileSize)
		}
	}
}

func (v *verifier) doVerifyObject(ctx context.Context, oid object.ID, path string, expectedLength int64) {
	if expectedLength < 0 {
		log.Debugf("verifying object %v", oid)
	} else {
		log.Debugf("verifying object %v (%v) with length %v", path, oid, expectedLength)
	}

	var length int64
	var err error

	length, _, err = v.om.VerifyObject(ctx, oid)
	if err != nil {
		v.reportError(path, fmt.Errorf("error verifying %v: %v", oid, err))
	}

	if expectedLength >= 0 && length != expectedLength {
		v.reportError(path, fmt.Errorf("invalid object length %q, %v, expected %v", oid, length, expectedLength))
	}

	if rand.Intn(100) < *verifyCommandFilesPercent {
		if err := v.readEntireObject(ctx, oid, path); err != nil {
			v.reportError(path, fmt.Errorf("error reading object %v: %v", oid, err))
		}
	}
}

func (v *verifier) readEntireObject(ctx context.Context, oid object.ID, path string) error {
	log.Debugf("reading object %v %v", oid, path)
	ctx = block.UsingBlockCache(ctx, false)

	// also read the entire file
	r, err := v.om.Open(ctx, oid)
	if err != nil {
		return err
	}
	defer r.Close() //nolint:errcheck

	_, err = io.Copy(ioutil.Discard, r)
	return err
}

func runVerifyCommand(ctx context.Context, rep *repo.Repository) error {
	mgr := snapshot.NewManager(rep)

	v := &verifier{
		mgr:       mgr,
		om:        rep.Objects,
		startTime: time.Now(),
		workQueue: parallelwork.NewQueue(),
		seen:      map[object.ID]bool{},
	}

	if err := enqueueRootsToVerify(ctx, v, mgr); err != nil {
		return err
	}

	v.workQueue.ProgressCallback = v.progressCallback
	v.workQueue.Process(*verifyCommandParallel)

	if len(v.errors) == 0 {
		return nil
	}

	return fmt.Errorf("encountered %v errors", len(v.errors))
}

func enqueueRootsToVerify(ctx context.Context, v *verifier, mgr *snapshot.Manager) error {
	manifests, err := loadSourceManifests(mgr, *verifyCommandAllSources, *verifyCommandSources)
	if err != nil {
		return err
	}

	for _, man := range manifests {
		path := fmt.Sprintf("%v@%v", man.Source, man.StartTime.Format(timeFormat))
		if man.RootEntry == nil {
			continue
		}

		if man.RootEntry.Type == fs.EntryTypeDirectory {
			v.enqueueVerifyDirectory(ctx, man.RootObjectID(), path)
		} else {
			v.enqueueVerifyObject(ctx, man.RootObjectID(), path, -1)
		}
	}

	for _, oidStr := range *verifyCommandDirObjectIDs {
		oid, err := parseObjectID(ctx, mgr, oidStr)
		if err != nil {
			return err
		}

		v.enqueueVerifyDirectory(ctx, oid, oidStr)
	}

	for _, oidStr := range *verifyCommandFileObjectIDs {
		oid, err := parseObjectID(ctx, mgr, oidStr)
		if err != nil {
			return err
		}

		v.enqueueVerifyObject(ctx, oid, oidStr, -1)
	}

	return nil
}

func loadSourceManifests(mgr *snapshot.Manager, all bool, sources []string) ([]*snapshot.Manifest, error) {
	var manifestIDs []string
	if *verifyCommandAllSources {
		manifestIDs = append(manifestIDs, mgr.ListSnapshotManifests(nil)...)
	} else {
		for _, srcStr := range *verifyCommandSources {
			src, err := snapshot.ParseSourceInfo(srcStr, getHostName(), getUserName())
			if err != nil {
				return nil, fmt.Errorf("error parsing %q: %v", srcStr, err)
			}
			manifestIDs = append(manifestIDs, mgr.ListSnapshotManifests(&src)...)
		}
	}
	return mgr.LoadSnapshots(manifestIDs)
}

func init() {
	verifyCommand.Action(repositoryAction(runVerifyCommand))
}
