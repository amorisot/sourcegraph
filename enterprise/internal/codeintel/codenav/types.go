package codenav

import (
	"strings"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/codenav/shared"
	uploadsshared "github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/uploads/shared"
	"github.com/sourcegraph/sourcegraph/lib/codeintel/precise"
)

// visibleUpload pairs an upload visible from the current target commit with the
// current target path and position matched to the data within the underlying index.
type visibleUpload struct {
	Upload                uploadsshared.Dump
	TargetPath            string
	TargetPosition        shared.Position
	TargetPathWithoutRoot string
}

type qualifiedMonikerSet struct {
	monikers       []precise.QualifiedMonikerData
	monikerHashMap map[string]struct{}
}

func newQualifiedMonikerSet() *qualifiedMonikerSet {
	return &qualifiedMonikerSet{
		monikerHashMap: map[string]struct{}{},
	}
}

// add the given qualified moniker to the set if it is distinct from all elements
// currently in the set.
func (s *qualifiedMonikerSet) add(qualifiedMoniker precise.QualifiedMonikerData) {
	monikerHash := strings.Join([]string{
		qualifiedMoniker.PackageInformationData.Name,
		qualifiedMoniker.PackageInformationData.Version,
		qualifiedMoniker.MonikerData.Scheme,
		qualifiedMoniker.PackageInformationData.Manager,
		qualifiedMoniker.MonikerData.Identifier,
	}, ":")

	if _, ok := s.monikerHashMap[monikerHash]; ok {
		return
	}

	s.monikerHashMap[monikerHash] = struct{}{}
	s.monikers = append(s.monikers, qualifiedMoniker)
}

type RequestArgs struct {
	RepositoryID int
	Commit       string
	Path         string
	Line         int
	Character    int
	Limit        int
	RawCursor    string
}

// DiagnosticAtUpload is a diagnostic from within a particular upload. The adjusted commit denotes
// the target commit for which the location was adjusted (the originally requested commit).
type DiagnosticAtUpload struct {
	shared.Diagnostic
	Dump           uploadsshared.Dump
	AdjustedCommit string
	AdjustedRange  shared.Range
}

// AdjustedCodeIntelligenceRange stores definition, reference, and hover information for all ranges
// within a block of lines. The definition and reference locations have been adjusted to fit the
// target (originally requested) commit.
type AdjustedCodeIntelligenceRange struct {
	Range           shared.Range
	Definitions     []shared.UploadLocation
	References      []shared.UploadLocation
	Implementations []shared.UploadLocation
	HoverText       string
}

// Cursor is a struct that holds the state necessary to resume a locations query from a second or
// subsequent request. This struct is used internally as a request-specific context object that is
// mutated as the locations request is fulfilled. This struct is serialized to JSON then base64
// encoded to make an opaque string that is handed to a future request to get the remainder of the
// result set.
type Cursor struct {
	Phase                string                `json:"a"` // ""/"local", "remote", or "done"
	VisibleUploads       []CursorVisibleUpload `json:"b"` // root uploads covering a particular code location
	LocalUploadOffset    int                   `json:"c"` // number of consumed visible uploads
	LocalLocationOffset  int                   `json:"d"` // offset within locations of VisibleUploads[LocalUploadOffset:]
	SymbolNames          []string              `json:"e"` // symbol names extracted from visible uploads
	SkipPathsByUploadID  map[int]string        `json:"f"` // paths to skip for particular uploads in the remote phase
	DefinitionIDs        []int                 `json:"g"` // identifiers of uploads defining relevant symbol names
	UploadIDs            []int                 `json:"h"` // current batch of uploads in which to search
	RemoteUploadOffset   int                   `json:"i"` // number of searched (to completion) uploads
	RemoteLocationOffset int                   `json:"j"` // offset within locations of the current upload batch
}

type CursorVisibleUpload struct {
	DumpID                int             `json:"k"`
	TargetPath            string          `json:"l"`
	TargetPosition        shared.Position `json:"m"`
	TargetPathWithoutRoot string          `json:"n"`
}

var exhaustedCursor = Cursor{Phase: "done"}

func (c Cursor) BumpLocalLocationOffset(n, totalCount int) Cursor {
	c.LocalLocationOffset += n
	if c.LocalLocationOffset >= totalCount {
		// We've consumed this upload completely. Skip it the next time we find
		// ourselves in this loop, and ensure that we start with a zero offset on
		// the next upload we process (if any).
		c.LocalUploadOffset++
		c.LocalLocationOffset = 0
	}

	return c
}

func (c Cursor) BumpRemoteUploadOffset(n, totalCount int) Cursor {
	c.RemoteUploadOffset += n
	if c.RemoteUploadOffset >= totalCount {
		// We've consumed all upload batches
		c.RemoteUploadOffset = -1
	}

	return c
}

func (c Cursor) BumpRemoteLocationOffset(n, totalCount int) Cursor {
	c.RemoteLocationOffset += n
	if c.RemoteLocationOffset >= totalCount {
		// We've consumed the locations for this set of uploads. Reset this slice value in the
		// cursor so that the next call to this function will query the new set of uploads to
		// search in while resolving the next page. We also ensure we start on a zero offset
		// for the next page of results for a fresh set of uploads (if any).
		c.UploadIDs = nil
		c.RemoteLocationOffset = 0
	}

	return c
}

// referencesCursor stores (enough of) the state of a previous References request used to
// calculate the offset into the result set to be returned by the current request.
type ReferencesCursor struct {
	CursorsToVisibleUploads []CursorToVisibleUpload        `json:"adjustedUploads"`
	OrderedMonikers         []precise.QualifiedMonikerData `json:"orderedMonikers"`
	Phase                   string                         `json:"phase"`
	LocalCursor             LocalCursor                    `json:"localCursor"`
	RemoteCursor            RemoteCursor                   `json:"remoteCursor"`
}

// ImplementationsCursor stores (enough of) the state of a previous Implementations request used to
// calculate the offset into the result set to be returned by the current request.
type ImplementationsCursor struct {
	CursorsToVisibleUploads       []CursorToVisibleUpload        `json:"visibleUploads"`
	OrderedImplementationMonikers []precise.QualifiedMonikerData `json:"orderedImplementationMonikers"`
	OrderedExportMonikers         []precise.QualifiedMonikerData `json:"orderedExportMonikers"`
	Phase                         string                         `json:"phase"`
	LocalCursor                   LocalCursor                    `json:"localCursor"`
	RemoteCursor                  RemoteCursor                   `json:"remoteCursor"`
}

// cursorAdjustedUpload
type CursorToVisibleUpload struct {
	DumpID                int             `json:"dumpID"`
	TargetPath            string          `json:"adjustedPath"`
	TargetPosition        shared.Position `json:"adjustedPosition"`
	TargetPathWithoutRoot string          `json:"adjustedPathInBundle"`
}

// localCursor is an upload offset and a location offset within that upload.
type LocalCursor struct {
	UploadOffset int `json:"uploadOffset"`
	// The location offset within the associated upload.
	LocationOffset int `json:"locationOffset"`
}

// RemoteCursor is an upload offset, the current batch of uploads, and a location offset within the batch of uploads.
type RemoteCursor struct {
	UploadOffset   int   `json:"batchOffset"`
	UploadBatchIDs []int `json:"uploadBatchIDs"`
	// The location offset within the associated batch of uploads.
	LocationOffset int `json:"locationOffset"`
}
