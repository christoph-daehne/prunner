// Package taskctl contains custom implementations of taskctl types
package taskctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/taskctl/taskctl/pkg/executor"
	"github.com/taskctl/taskctl/pkg/output"
	"github.com/taskctl/taskctl/pkg/runner"
	"github.com/taskctl/taskctl/pkg/task"
	"github.com/taskctl/taskctl/pkg/variables"
)

// TaskRunner run tasks
type TaskRunner struct {
	contexts  map[string]*runner.ExecutionContext
	variables variables.Container
	env       variables.Container

	ctx         context.Context
	cancelFunc  context.CancelFunc
	cancelMutex sync.RWMutex
	canceling   bool
	doneCh      chan struct{}

	compiler *runner.TaskCompiler

	Stdin          io.Reader
	Stdout, Stderr io.Writer
	OutputFormat   string

	cleanupList sync.Map

	taskOutputsMutex sync.RWMutex
	taskOutputs      map[*task.Task]*taskOutput
	outputStore      *OutputStore
}

// NewTaskRunner creates new TaskRunner instance
func NewTaskRunner(outputStore *OutputStore, opts ...Opts) (*TaskRunner, error) {
	r := &TaskRunner{
		compiler:     runner.NewTaskCompiler(),
		OutputFormat: output.FormatRaw,
		Stdin:        os.Stdin,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		variables:    variables.NewVariables(),
		env:          variables.NewVariables(),
		doneCh:       make(chan struct{}, 1),

		taskOutputs: make(map[*task.Task]*taskOutput),
		outputStore: outputStore,
	}

	r.ctx, r.cancelFunc = context.WithCancel(context.Background())

	for _, o := range opts {
		o(r)
	}

	r.env = variables.FromMap(map[string]string{"ARGS": r.variables.Get("Args").(string)})

	return r, nil
}

// SetContexts sets task runner's contexts
func (r *TaskRunner) SetContexts(contexts map[string]*runner.ExecutionContext) *TaskRunner {
	r.contexts = contexts

	return r
}

// SetVariables sets task runner's variables
func (r *TaskRunner) SetVariables(vars variables.Container) *TaskRunner {
	r.variables = vars

	return r
}

// Run run provided task.
// TaskRunner first compiles task into linked list of Jobs, then passes those jobs to Executor
func (r *TaskRunner) Run(t *task.Task) error {
	defer func() {
		r.cancelMutex.RLock()
		if r.canceling {
			close(r.doneCh)
		}
		r.cancelMutex.RUnlock()
	}()

	if err := r.ctx.Err(); err != nil {
		return err
	}

	execContext, err := r.contextForTask(t)
	if err != nil {
		return err
	}

	var stdin io.Reader
	if t.Interactive {
		stdin = r.Stdin
	}

	defer func() {
		err = execContext.After()
		if err != nil {
			logrus.Error(err)
		}

		if !t.Errored && !t.Skipped {
			t.ExitCode = 0
		}
	}()

	vars := r.variables.Merge(t.Variables)

	env := r.env.Merge(execContext.Env)
	env = env.With("TASK_NAME", t.Name)
	env = env.Merge(t.Env)

	meets, err := r.checkTaskCondition(t)
	if err != nil {
		return err
	}

	if !meets {
		logrus.Infof("task %s was skipped", t.Name)
		t.Skipped = true
		return nil
	}

	err = r.before(r.ctx, t, env, vars)
	if err != nil {
		return err
	}

	// Modification: add a temporary task output while the task is running
	r.taskOutputsMutex.Lock()
	out := newTaskOutput()
	r.taskOutputs[t] = out
	r.taskOutputsMutex.Unlock()

	defer func() {
		r.taskOutputsMutex.Lock()
		// Flush task outputs
		r.taskOutputs[t].stdout.Finish()
		r.taskOutputs[t].stderr.Finish()
		// Remove task output after finishing task
		delete(r.taskOutputs, t)
		r.taskOutputsMutex.Unlock()
	}()

	jobID := t.Variables.Get("jobID").(string)

	stdoutStorer, err := r.outputStore.Writer(jobID, t.Name, "stdout")
	if err != nil {
		return err
	}
	defer func() {
		stdoutStorer.Close()
	}()

	stderrStorer, err := r.outputStore.Writer(jobID, t.Name, "stderr")
	if err != nil {
		return err
	}
	defer func() {
		stderrStorer.Close()
	}()

	job, err := r.compiler.CompileTask(
		t,
		execContext,
		stdin,
		io.MultiWriter(
			out.stdout,
			&t.Log.Stdout,
			stdoutStorer,
		),
		io.MultiWriter(
			out.stderr,
			&t.Log.Stderr,
			stderrStorer,
		),
		env,
		vars,
	)
	if err != nil {
		return err
	}

	err = r.execute(r.ctx, t, job)
	if err != nil {
		return err
	}
	r.storeTaskOutput(t)

	return r.after(r.ctx, t, env, vars)
}

type taskOutput struct {
	stdout *LineWriter
	stderr *LineWriter
}

func newTaskOutput() *taskOutput {
	return &taskOutput{
		stdout: &LineWriter{},
		stderr: &LineWriter{},
	}
}

func (r *TaskRunner) CurrentTaskOutput(t *task.Task) (stdout [][]byte, stderr [][]byte, ok bool) {
	r.taskOutputsMutex.RLock()
	defer r.taskOutputsMutex.RUnlock()

	out, ok := r.taskOutputs[t]
	if !ok {
		return nil, nil, false
	}

	return out.stdout.Lines(), out.stderr.Lines(), true
}

// Cancel cancels execution
func (r *TaskRunner) Cancel() {
	r.cancelMutex.Lock()
	if !r.canceling {
		r.canceling = true
		defer logrus.Debug("runner has been cancelled")
		r.cancelFunc()
	}
	r.cancelMutex.Unlock()
	<-r.doneCh
}

// Finish makes cleanup tasks over contexts
func (r *TaskRunner) Finish() {
	r.cleanupList.Range(func(key, value interface{}) bool {
		value.(*runner.ExecutionContext).Down()
		return true
	})
	output.Close()
}

// WithVariable adds variable to task runner's variables list.
// It creates new instance of variables container.
func (r *TaskRunner) WithVariable(key, value string) *TaskRunner {
	r.variables = r.variables.With(key, value)

	return r
}

func (r *TaskRunner) before(ctx context.Context, t *task.Task, env, vars variables.Container) error {
	if len(t.Before) == 0 {
		return nil
	}

	execContext, err := r.contextForTask(t)
	if err != nil {
		return err
	}

	for _, command := range t.Before {
		job, err := r.compiler.CompileCommand(command, execContext, t.Dir, t.Timeout, nil, r.Stdout, r.Stderr, env, vars)
		if err != nil {
			return fmt.Errorf("\"before\" command compilation failed: %w", err)
		}

		exec, err := executor.NewDefaultExecutor(job.Stdin, job.Stdout, job.Stderr)
		if err != nil {
			return err
		}

		_, err = exec.Execute(ctx, job)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *TaskRunner) after(ctx context.Context, t *task.Task, env, vars variables.Container) error {
	if len(t.After) == 0 {
		return nil
	}

	execContext, err := r.contextForTask(t)
	if err != nil {
		return err
	}

	for _, command := range t.After {
		job, err := r.compiler.CompileCommand(command, execContext, t.Dir, t.Timeout, nil, r.Stdout, r.Stderr, env, vars)
		if err != nil {
			return fmt.Errorf("\"after\" command compilation failed: %w", err)
		}

		exec, err := executor.NewDefaultExecutor(job.Stdin, job.Stdout, job.Stderr)
		if err != nil {
			return err
		}

		_, err = exec.Execute(ctx, job)
		if err != nil {
			logrus.Warning(err)
		}
	}

	return nil
}

func (r *TaskRunner) contextForTask(t *task.Task) (c *runner.ExecutionContext, err error) {
	if t.Context == "" {
		c = runner.DefaultContext()
	} else {
		var ok bool
		c, ok = r.contexts[t.Context]
		if !ok {
			return nil, fmt.Errorf("no such context %s", t.Context)
		}

		r.cleanupList.Store(t.Context, c)
	}

	err = c.Up()
	if err != nil {
		return nil, err
	}

	err = c.Before()
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (r *TaskRunner) checkTaskCondition(t *task.Task) (bool, error) {
	if t.Condition == "" {
		return true, nil
	}

	executionContext, err := r.contextForTask(t)
	if err != nil {
		return false, err
	}

	job, err := r.compiler.CompileCommand(t.Condition, executionContext, t.Dir, t.Timeout, nil, r.Stdout, r.Stderr, r.env, r.variables)
	if err != nil {
		return false, err
	}

	exec, err := executor.NewDefaultExecutor(job.Stdin, job.Stdout, job.Stderr)
	if err != nil {
		return false, err
	}

	_, err = exec.Execute(context.Background(), job)
	if err != nil {
		if _, ok := executor.IsExitStatus(err); ok {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func (r *TaskRunner) storeTaskOutput(t *task.Task) {
	var envVarName string
	varName := fmt.Sprintf("Tasks.%s.Output", strings.Title(t.Name))

	if t.ExportAs == "" {
		envVarName = fmt.Sprintf("%s_OUTPUT", strings.ToUpper(t.Name))
		envVarName = regexp.MustCompile("[^a-zA-Z0-9_]").ReplaceAllString(envVarName, "_")
	} else {
		envVarName = t.ExportAs
	}

	r.env.Set(envVarName, t.Log.Stdout.String())
	r.variables.Set(varName, t.Log.Stdout.String())
}

func (r *TaskRunner) execute(ctx context.Context, t *task.Task, job *executor.Job) error {
	exec, err := executor.NewDefaultExecutor(job.Stdin, job.Stdout, job.Stderr)
	if err != nil {
		return err
	}

	t.Start = time.Now()
	var prevOutput []byte
	for nextJob := job; nextJob != nil; nextJob = nextJob.Next {
		var err error
		nextJob.Vars.Set("Output", string(prevOutput))

		prevOutput, err = exec.Execute(ctx, nextJob)
		if err != nil {
			logrus.Debug(err.Error())
			if status, ok := executor.IsExitStatus(err); ok {
				t.ExitCode = int16(status)
				if t.AllowFailure {
					continue
				}
			}
			t.Errored = true
			t.Error = err
			t.End = time.Now()
			return t.Error
		}
	}
	t.End = time.Now()

	return nil
}

// Opts is a task runner configuration function.
type Opts func(*TaskRunner)