package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kballard/go-shellquote"

	"github.com/sourcegraph/sourcegraph/enterprise/cmd/batcheshelper/log"
	batcheslib "github.com/sourcegraph/sourcegraph/lib/batches"
	"github.com/sourcegraph/sourcegraph/lib/batches/execution"
	"github.com/sourcegraph/sourcegraph/lib/batches/git"
	"github.com/sourcegraph/sourcegraph/lib/batches/template"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

// Pre prepares the workspace for the Batch Change step.
func Pre(
	ctx context.Context,
	logger *log.Logger,
	stepIdx int,
	executionInput batcheslib.WorkspacesExecutionInput,
	previousResult execution.AfterStepResult,
	workspaceFilesPath string,
) error {
	// Resolve step.Env given the current environment.
	step := executionInput.Steps[stepIdx]
	stepEnv, err := step.Env.Resolve(os.Environ())
	if err != nil {
		return errors.Wrap(err, "failed to resolve step env")
	}
	stepContext, err := getStepContext(executionInput, previousResult)
	if err != nil {
		return err
	}

	// Configures copying of the files to be used by the step.
	var fileMountsPreamble string

	// Check if the step needs to be skipped.
	cond, err := template.EvalStepCondition(step.IfCondition(), &stepContext)
	if err != nil {
		return errors.Wrap(err, "failed to evaluate step condition")
	}

	if !cond {
		// Write the skip event to the log.
		if err = logger.WriteEvent(batcheslib.LogEventOperationTaskStepSkipped, batcheslib.LogEventStatusProgress, &batcheslib.TaskStepMetadata{
			Step: stepIdx + 1,
		}); err != nil {
			return err
		}
		fmt.Println("Skipping step")

		// Write the step result file with the skipped flag set.
		stepResult := execution.AfterStepResult{
			Version: 2,
			Skipped: true,
		}
		stepResultBytes, err := json.Marshal(stepResult)
		if err != nil {
			return errors.Wrap(err, "marshalling step result")
		}
		if err = os.WriteFile(fmt.Sprintf("step%d.json", stepIdx), stepResultBytes, os.ModePerm); err != nil {
			return errors.Wrap(err, "failed to write step result file")
		}

		// Bail out early.
		os.Exit(0)
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return errors.Wrap(err, "getting working directory")
		}
		tmpFileDir := filepath.Join(wd, fmt.Sprintf("step%dfiles", stepIdx))
		if err = os.Mkdir(tmpFileDir, os.ModePerm); err != nil {
			return errors.Wrap(err, "failed to create directory for file mounts")
		}

		// Parse and render the step.Files.
		filesToMount, err := createFilesToMount(tmpFileDir, step, &stepContext)
		if err != nil {
			return errors.Wrap(err, "failed to create files to mount")
		}
		for path, file := range filesToMount {
			// TODO: Does file.Name() work?
			fileMountsPreamble += fmt.Sprintf("%s\n", shellquote.Join("cp", file.Name(), path))
			fileMountsPreamble += fmt.Sprintf("%s\n", shellquote.Join("chmod", "+x", path))
		}

		// Mount any paths on the local system to the docker container. The paths have already been validated during parsing.
		for _, mount := range step.Mount {
			workspaceFilePath, err := getAbsoluteMountPath(workspaceFilesPath, mount.Path)
			if err != nil {
				return errors.Wrap(err, "getAbsoluteMountPath")
			}
			fileMountsPreamble += fmt.Sprintf("%s\n", shellquote.Join("cp", "-r", workspaceFilePath, mount.Mountpoint))
			fileMountsPreamble += fmt.Sprintf("%s\n", shellquote.Join("chmod", "-R", "+x", mount.Mountpoint))
		}
	}

	// Render the step.Env template.
	env, err := template.RenderStepMap(stepEnv, &stepContext)
	if err != nil {
		return errors.Wrap(err, "failed to render step env")
	}

	// Write the event to the log. Ensure environment variables will be rendered.
	if err = logger.WriteEvent(batcheslib.LogEventOperationTaskStep, batcheslib.LogEventStatusStarted, &batcheslib.TaskStepMetadata{
		Step: stepIdx + 1,
		Env:  env,
	}); err != nil {
		return err
	}

	// Render the step.Run template.
	var runScript bytes.Buffer
	if err = template.RenderStepTemplate("step-run", step.Run, &runScript, &stepContext); err != nil {
		return errors.Wrap(err, "failed to render step.run")
	}

	// Create the environment preamble for the step script.
	envPreamble := ""
	for k, v := range env {
		envPreamble += shellquote.Join("export", fmt.Sprintf("%s=%s", k, v))
		envPreamble += "\n"
	}

	stepScriptPath := fmt.Sprintf("step%d.sh", stepIdx)
	fullScript := []byte(envPreamble + fileMountsPreamble + runScript.String())
	if err = os.WriteFile(stepScriptPath, fullScript, os.ModePerm); err != nil {
		return errors.Wrap(err, "failed to write step script file")
	}
	if _, err = exec.CommandContext(ctx, "chmod", "+x", stepScriptPath).CombinedOutput(); err != nil {
		return errors.Wrap(err, "failed to chmod step script file")
	}

	return nil
}

func getStepContext(executionInput batcheslib.WorkspacesExecutionInput, previousResult execution.AfterStepResult) (template.StepContext, error) {
	changes, err := git.ChangesInDiff(previousResult.Diff)
	if err != nil {
		return template.StepContext{}, errors.Wrap(err, "failed to compute changes")
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
	}
	return stepContext, nil
}

// createFilesToMount creates temporary files with the contents of Step.Files
// that are to be mounted into the container that executes the step.
// TODO: Remove these files in the `after` step.
func createFilesToMount(tempDir string, step batcheslib.Step, stepContext *template.StepContext) (map[string]*os.File, error) {
	// Parse and render the step.Files.
	files, err := template.RenderStepMap(step.Files, stepContext)
	if err != nil {
		return nil, errors.Wrap(err, "parsing step files")
	}

	// Create temp files with the rendered content of step.Files so that we
	// can mount them into the container.
	filesToMount := make(map[string]*os.File, len(files))
	for name, content := range files {
		fp, err := os.CreateTemp(tempDir, "")
		if err != nil {
			return nil, errors.Wrap(err, "creating temporary file")
		}

		if _, err = fp.WriteString(content); err != nil {
			return nil, errors.Wrap(err, "writing to temporary file")
		}

		if err = fp.Close(); err != nil {
			return nil, errors.Wrap(err, "closing temporary file")
		}

		filesToMount[name] = fp
	}

	return filesToMount, nil
}

func getAbsoluteMountPath(batchSpecDir string, mountPath string) (string, error) {
	p := mountPath
	if !filepath.IsAbs(p) {
		// Try to build the absolute path since Docker will only mount absolute paths
		p = filepath.Join(batchSpecDir, p)
	}
	pathInfo, err := os.Stat(p)
	if os.IsNotExist(err) {
		return "", errors.Newf("mount path %s does not exist", p)
	} else if err != nil {
		return "", errors.Wrap(err, "mount path validation")
	}
	if !strings.HasPrefix(p, batchSpecDir) {
		return "", errors.Newf("mount path %s is not in the same directory or subdirectory as the batch spec", mountPath)
	}
	// Mounting a directory on Docker must end with the separator. So, append the file separator to make
	// users' lives easier.
	if pathInfo.IsDir() && !strings.HasSuffix(p, string(filepath.Separator)) {
		p += string(filepath.Separator)
	}
	return p, nil
}