package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sourcegraph/sourcegraph/enterprise/cmd/batcheshelper/log"
	batcheslib "github.com/sourcegraph/sourcegraph/lib/batches"
	"github.com/sourcegraph/sourcegraph/lib/batches/execution"
	"github.com/sourcegraph/sourcegraph/lib/batches/execution/cache"
	"github.com/sourcegraph/sourcegraph/lib/batches/git"
	"github.com/sourcegraph/sourcegraph/lib/batches/template"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

// Post TODO
func Post(
	ctx context.Context,
	logger *log.Logger,
	stepIdx int,
	executionInput batcheslib.WorkspacesExecutionInput,
	previousResult execution.AfterStepResult,
	workspaceFilesPath string,
) error {
	step := executionInput.Steps[stepIdx]

	// Generate the diff.
	if _, err := runGitCmd(ctx, "git", "add", "--all"); err != nil {
		return errors.Wrap(err, "git add --all failed")
	}
	diff, err := runGitCmd(ctx, "git", "diff", "--cached", "--no-prefix", "--binary")
	if err != nil {
		return errors.Wrap(err, "git diff --cached --no-prefix --binary failed")
	}

	// Read the stdout of the current step.
	stdout, err := os.ReadFile(fmt.Sprintf("stdout%d.log", stepIdx))
	if err != nil {
		return errors.Wrap(err, "failed to read stdout file")
	}
	// Read the stderr of the current step.
	stderr, err := os.ReadFile(fmt.Sprintf("stderr%d.log", stepIdx))
	if err != nil {
		return errors.Wrap(err, "failed to read stderr file")
	}

	// Build the step result.
	stepResult := execution.AfterStepResult{
		Version:   2,
		Stdout:    string(stdout),
		Stderr:    string(stderr),
		StepIndex: stepIdx,
		Diff:      diff,
		// Those will be set below.
		Outputs: make(map[string]interface{}),
	}

	// Render the step outputs.
	changes, err := git.ChangesInDiff(previousResult.Diff)
	if err != nil {
		return errors.Wrap(err, "failed to get changes in diff")
	}
	outputs := previousResult.Outputs
	if outputs == nil {
		outputs = make(map[string]any)
	}
	stepContext := template.StepContext{
		BatchChange: executionInput.BatchChangeAttributes,
		Repository: template.Repository{
			Name:        executionInput.Repository.Name,
			Branch:      executionInput.Branch.Name,
			FileMatches: executionInput.SearchResultPaths,
		},
		Outputs: outputs,
		Steps: template.StepsContext{
			Path:    executionInput.Path,
			Changes: changes,
		},
		PreviousStep: previousResult,
		Step:         stepResult,
	}

	// Render and evaluate outputs.
	if err = batcheslib.SetOutputs(step.Outputs, outputs, &stepContext); err != nil {
		return errors.Wrap(err, "setting outputs")
	}
	for k, v := range outputs {
		stepResult.Outputs[k] = v
	}

	// Serialize the step result to disk.
	cntnt, err := json.Marshal(stepResult)
	if err != nil {
		return errors.Wrap(err, "marshalling step result")
	}
	if err = os.WriteFile(fmt.Sprintf("step%d.json", stepIdx), cntnt, os.ModePerm); err != nil {
		return errors.Wrap(err, "failed to write step result file")
	}

	key := cache.KeyForWorkspace(
		&executionInput.BatchChangeAttributes,
		batcheslib.Repository{
			ID:          executionInput.Repository.ID,
			Name:        executionInput.Repository.Name,
			BaseRef:     executionInput.Branch.Name,
			BaseRev:     executionInput.Branch.Target.OID,
			FileMatches: executionInput.SearchResultPaths,
		},
		executionInput.Path,
		os.Environ(),
		executionInput.OnlyFetchWorkspace,
		executionInput.Steps,
		stepIdx,
		fileMetadataRetriever{workingDirectory: workspaceFilesPath},
	)

	k, err := key.Key()
	if err != nil {
		return errors.Wrap(err, "failed to compute cache key")
	}

	err = logger.WriteEvent(
		batcheslib.LogEventOperationCacheAfterStepResult,
		batcheslib.LogEventStatusSuccess,
		&batcheslib.CacheAfterStepResultMetadata{Key: k, Value: stepResult},
	)
	if err != nil {
		return err
	}

	return nil
}

func runGitCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = "repository"

	return cmd.Output()
}

type fileMetadataRetriever struct {
	workingDirectory string
}

var _ cache.MetadataRetriever = fileMetadataRetriever{}

func (f fileMetadataRetriever) Get(steps []batcheslib.Step) ([]cache.MountMetadata, error) {
	var mountsMetadata []cache.MountMetadata
	for _, step := range steps {
		// Build up the metadata for each mount for each step
		for _, mount := range step.Mount {
			metadata, err := f.getMountMetadata(f.workingDirectory, mount.Path)
			if err != nil {
				return nil, err
			}
			// A mount could be a directory containing multiple files
			mountsMetadata = append(mountsMetadata, metadata...)
		}
	}
	return mountsMetadata, nil
}

func (f fileMetadataRetriever) getMountMetadata(baseDir string, path string) ([]cache.MountMetadata, error) {
	fullPath := path
	if !filepath.IsAbs(path) {
		fullPath = filepath.Join(baseDir, path)
	}
	info, err := os.Stat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.Newf("path %s does not exist", path)
	} else if err != nil {
		return nil, err
	}
	var metadata []cache.MountMetadata
	if info.IsDir() {
		dirMetadata, err := f.getDirectoryMountMetadata(fullPath)
		if err != nil {
			return nil, err
		}
		metadata = append(metadata, dirMetadata...)
	} else {
		relativePath, err := filepath.Rel(f.workingDirectory, fullPath)
		if err != nil {
			return nil, err
		}
		metadata = append(metadata, cache.MountMetadata{Path: relativePath, Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	return metadata, nil
}

// getDirectoryMountMetadata reads all the files in the directory with the given
// path and returns the cache.MountMetadata for all of them.
func (f fileMetadataRetriever) getDirectoryMountMetadata(path string) ([]cache.MountMetadata, error) {
	dir, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var metadata []cache.MountMetadata
	for _, dirEntry := range dir {
		// Go back to the very start. Need to get the FileInfo again for the new path and figure out if it is a
		// directory or a file.
		fileMetadata, err := f.getMountMetadata(path, dirEntry.Name())
		if err != nil {
			return nil, err
		}
		metadata = append(metadata, fileMetadata...)
	}
	return metadata, nil
}
