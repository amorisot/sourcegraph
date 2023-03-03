package graphqlbackend

import (
	"context"
	"path/filepath"

	"github.com/sourcegraph/log"
	"github.com/sourcegraph/sourcegraph/internal/auth"
	"github.com/sourcegraph/sourcegraph/internal/conf/deploy"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/service/servegit"
	"github.com/sourcegraph/sourcegraph/internal/singleprogram/filepicker"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

type localResolver struct {
	logger log.Logger
	db     database.DB
}

func (r *localResolver) checkLocalDirectoryAccess(ctx context.Context) error {
	if !deploy.IsDeployTypeSingleProgram(deploy.Type()) {
		return errors.New("local directory APIs only available on Sourcegraph App")
	}

	return auth.CheckCurrentUserIsSiteAdmin(ctx, r.db)
}

func (r *localResolver) LocalDirectoryPicker(ctx context.Context) (*localDirectoryResolver, error) {
	// 🚨 SECURITY: Only site admins on app may use API which accesses local filesystem.
	if err := r.checkLocalDirectoryAccess(ctx); err != nil {
		return nil, err
	}

	picker, ok := filepicker.Lookup(r.logger)
	if !ok {
		return nil, errors.New("filepicker is not available")
	}

	path, err := picker(ctx)
	if err != nil {
		return nil, err
	}

	return &localDirectoryResolver{path: path}, nil
}

func (r *localResolver) LocalDirectory(ctx context.Context, args *struct{ Dir string }) (*localDirectoryResolver, error) {
	// 🚨 SECURITY: Only site admins on app may use API which accesses local filesystem.
	if err := r.checkLocalDirectoryAccess(ctx); err != nil {
		return nil, err
	}

	path, err := filepath.Abs(args.Dir)
	if err != nil {
		return nil, err
	}

	return &localDirectoryResolver{path: path}, nil
}

type localDirectoryResolver struct {
	path string
}

func (r *localDirectoryResolver) Path() string {
	return r.path
}

func (r *localDirectoryResolver) Repositories() ([]localRepositoryResolver, error) {
	var c servegit.Config
	c.Load()
	c.Root = r.path

	srv := &servegit.Serve{
		Config: c,
		Logger: log.Scoped("serve", ""),
	}

	repos, err := srv.Repos()
	if err != nil {
		return nil, err
	}

	local := make([]localRepositoryResolver, 0, len(repos))
	for _, repo := range repos {
		local = append(local, localRepositoryResolver{
			name: repo.Name,
			path: filepath.Join(r.path, repo.Name), // TODO(keegan) this is not always correct
		})
	}

	return local, nil
}

type localRepositoryResolver struct {
	name string
	path string
}

func (r localRepositoryResolver) Name() string {
	return r.name
}

func (r localRepositoryResolver) Path() string {
	return r.path
}
