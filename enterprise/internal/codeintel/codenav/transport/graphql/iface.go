package graphql

import (
	"context"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/codenav"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/codenav/shared"
	uploadsshared "github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/uploads/shared"
)

type CodeNavService interface {
	GetHover(ctx context.Context, args codenav.RequestArgs, requestState codenav.RequestState) (_ string, _ shared.Range, _ bool, err error)
	NewGetReferences(ctx context.Context, args codenav.RequestArgs, requestState codenav.RequestState, cursor codenav.GenericCursor) (_ []shared.UploadLocation, nextCursor codenav.GenericCursor, err error)
	NewGetImplementations(ctx context.Context, args codenav.RequestArgs, requestState codenav.RequestState, cursor codenav.GenericCursor) (_ []shared.UploadLocation, nextCursor codenav.GenericCursor, err error)
	NewGetPrototypes(ctx context.Context, args codenav.RequestArgs, requestState codenav.RequestState, cursor codenav.GenericCursor) (_ []shared.UploadLocation, nextCursor codenav.GenericCursor, err error)
	NewGetDefinitions(ctx context.Context, args codenav.RequestArgs, requestState codenav.RequestState) (_ []shared.UploadLocation, err error)
	GetDiagnostics(ctx context.Context, args codenav.RequestArgs, requestState codenav.RequestState) (diagnosticsAtUploads []codenav.DiagnosticAtUpload, _ int, err error)
	GetRanges(ctx context.Context, args codenav.RequestArgs, requestState codenav.RequestState, startLine, endLine int) (adjustedRanges []codenav.AdjustedCodeIntelligenceRange, err error)
	GetStencil(ctx context.Context, args codenav.RequestArgs, requestState codenav.RequestState) (adjustedRanges []shared.Range, err error)
	GetClosestDumpsForBlob(ctx context.Context, repositoryID int, commit, path string, exactPath bool, indexer string) (_ []uploadsshared.Dump, err error)
	VisibleUploadsForPath(ctx context.Context, requestState codenav.RequestState) ([]uploadsshared.Dump, error)
	SnapshotForDocument(ctx context.Context, repositoryID int, commit, path string, uploadID int) (data []shared.SnapshotData, err error)
}

type AutoIndexingService interface {
	QueueRepoRev(ctx context.Context, repositoryID int, rev string) error
}
