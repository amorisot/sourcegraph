package codenav

import (
	"context"
	"sort"
	"strings"

	"github.com/sourcegraph/scip/bindings/go/scip"
	"go.opentelemetry.io/otel/attribute"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/codenav/shared"
	"github.com/sourcegraph/sourcegraph/internal/observation"
	"github.com/sourcegraph/sourcegraph/lib/codeintel/precise"
)

func (s *Service) NewGetDefinitions(ctx context.Context, args RequestArgs, requestState RequestState) (_ []shared.UploadLocation, err error) {
	locations, _, err := s.gatherLocations(
		ctx,
		args,
		requestState,
		s.operations.getDefinitions,
		GenericCursor{},
		"definitions",
		true,
		s.makeDefinitionUploadFactory(requestState),
		s.lsifstore.ExtractDefinitionLocationsFromPosition,
	)

	return locations, err
}

func (s *Service) NewGetReferences(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx,
		args,
		requestState,
		s.operations.getReferences,
		cursor,
		"references",
		false,
		s.makeReferencesUploadFactory(args, requestState),
		s.lsifstore.ExtractReferenceLocationsFromPosition,
	)
}

func (s *Service) NewGetImplementations(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx,
		args,
		requestState,
		s.operations.getImplementations,
		cursor,
		"implementations",
		false,
		s.makeReferencesUploadFactory(args, requestState),
		s.lsifstore.ExtractImplementationLocationsFromPosition,
	)
}

func (s *Service) NewGetPrototypes(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx,
		args,
		requestState,
		s.operations.getPrototypes,
		cursor,
		"definitions", // N.B.
		false,
		s.makeDefinitionUploadFactory(requestState),
		s.lsifstore.ExtractPrototypeLocationsFromPosition,
	)
}

//
//

func (s *Service) makeDefinitionUploadFactory(requestState RequestState) getSearchableUploadIDsFunc {
	return func(ctx context.Context, monikers []precise.QualifiedMonikerData) ([]int, error) {
		uploads, err := s.getUploadsWithDefinitionsForMonikers(ctx, monikers, requestState)
		if err != nil {
			return nil, err
		}

		var ids []int
		for _, u := range uploads {
			ids = append(ids, u.ID)
		}
		return ids, nil
	}
}

func (s *Service) makeReferencesUploadFactory(args RequestArgs, requestState RequestState) getSearchableUploadIDsFunc {
	return func(ctx context.Context, monikers []precise.QualifiedMonikerData) ([]int, error) {
		uploads, err := s.getUploadsWithDefinitionsForMonikers(ctx, monikers, requestState)
		if err != nil {
			return nil, err
		}

		var ids []int
		for _, u := range uploads {
			ids = append(ids, u.ID)
		}

		referenceIDs, _, _, err := s.uploadSvc.GetUploadIDsWithReferences(
			ctx,
			monikers,
			ids,
			args.RepositoryID,
			args.Commit,
			requestState.maximumIndexesPerMonikerSearch,
			0, // offset
		)
		if err != nil {
			return nil, err
		}
		// Fetch the upload records we don't currently have hydrated and insert them into the map
		if _, err := s.getUploadsByIDs(ctx, referenceIDs, requestState); err != nil {
			return nil, err
		}

		return append(ids, referenceIDs...), nil
	}
}

//
//

type getSearchableUploadIDsFunc func(ctx context.Context, monikers []precise.QualifiedMonikerData) ([]int, error)
type getLocationsFromPositionFunc func(ctx context.Context, bundleID int, path string, line, character, limit, offset int) ([]shared.Location, int, []string, error)

const skipPrefix = "lsif ."

var exhaustedCursor = GenericCursor{Phase: "done"}

func (s *Service) gatherLocations(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	operation *observation.Operation,
	cursor GenericCursor,
	tableName string,
	stopAfterFirstResult bool,
	getSearchableUploadIDs getSearchableUploadIDsFunc,
	getLocationsFromPosition getLocationsFromPositionFunc,
) (_ []shared.UploadLocation, _ GenericCursor, err error) {
	ctx, trace, endObservation := observeResolver(ctx, &err, operation, serviceObserverThreshold, observation.Args{Attrs: []attribute.KeyValue{
		attribute.Int("repositoryID", args.RepositoryID),
		attribute.String("commit", args.Commit),
		attribute.String("path", args.Path),
		attribute.Int("numUploads", len(requestState.GetCacheUploads())),
		attribute.String("uploads", uploadIDsToString(requestState.GetCacheUploads())),
		attribute.Int("line", args.Line),
		attribute.Int("character", args.Character),
	}})
	defer endObservation()

	// Determine the set of visible uploads for the source commit
	visibleUploads, cursorsToVisibleUploads, err := s.getVisibleUploadsFromCursor(ctx, args.Line, args.Character, &cursor.CursorsToVisibleUploads, requestState)
	if err != nil {
		return nil, cursor, err
	}
	cursor.CursorsToVisibleUploads = cursorsToVisibleUploads

	var allLocations []shared.UploadLocation
	allSymbols := map[string]struct{}{}
	skipPaths := map[int]string{}

	for i := range visibleUploads {
		trace.AddEvent("TODO Domain Owner", attribute.Int("uploadID", visibleUploads[i].Upload.ID))

		locations, _, uploadSymbols, err := getLocationsFromPosition(
			ctx,
			visibleUploads[i].Upload.ID,
			visibleUploads[i].TargetPathWithoutRoot,
			visibleUploads[i].TargetPosition.Line,
			visibleUploads[i].TargetPosition.Character,
			args.Limit,
			0,
		)
		if err != nil {
			return nil, GenericCursor{}, err
		}
		if len(locations) > 0 {
			uploadLocations, err := s.getUploadLocations(ctx, args, requestState, locations, true)
			if err != nil {
				return nil, GenericCursor{}, err
			}
			if stopAfterFirstResult {
				return uploadLocations, exhaustedCursor, nil
			}

			allLocations = append(allLocations, uploadLocations...)
			skipPaths[visibleUploads[i].Upload.ID] = visibleUploads[i].TargetPathWithoutRoot
		}

		for _, symbolName := range uploadSymbols {
			if !strings.HasPrefix(symbolName, skipPrefix) {
				allSymbols[symbolName] = struct{}{}
			}
		}
	}

	var symbolNames []string
	for symbolName := range allSymbols {
		symbolNames = append(symbolNames, symbolName)
	}
	sort.Strings(symbolNames)

	monikers, err := symbolsToMonikers(symbolNames)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	uploadIDs, err := getSearchableUploadIDs(ctx, monikers)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	monikerArgs := make([]precise.MonikerData, 0, len(monikers))
	for _, moniker := range monikers {
		monikerArgs = append(monikerArgs, moniker.MonikerData)
	}

	locations, _, err := s.lsifstore.GetMinimalBulkMonikerLocations(ctx, tableName, uploadIDs, skipPaths, monikerArgs, 10000, 0)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// Adjust locations back to target commit
	adjustedLocations, err := s.getUploadLocations(ctx, args, requestState, locations, false)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	return append(allLocations, adjustedLocations...), exhaustedCursor, nil
}

func symbolsToMonikers(symbolNames []string) ([]precise.QualifiedMonikerData, error) {
	var monikers []precise.QualifiedMonikerData
	for _, symbolName := range symbolNames {
		parsedSymbol, err := scip.ParseSymbol(symbolName)
		if err != nil {
			return nil, err
		}

		monikers = append(monikers, precise.QualifiedMonikerData{
			MonikerData: precise.MonikerData{
				Scheme:     parsedSymbol.Scheme,
				Identifier: symbolName,
			},
			PackageInformationData: precise.PackageInformationData{
				Manager: parsedSymbol.Package.Manager,
				Name:    parsedSymbol.Package.Name,
				Version: parsedSymbol.Package.Version,
			},
		})
	}

	return monikers, nil
}
