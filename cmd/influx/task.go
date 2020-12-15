package main

import (
	"context"
	"fmt"
	"github.com/influxdata/influxdb/v2/tenant"
	"io"
	"os"
	"time"

	"github.com/influxdata/influxdb/v2"
	"github.com/influxdata/influxdb/v2/cmd/influx/internal"
	"github.com/influxdata/influxdb/v2/http"
	"github.com/spf13/cobra"
)

type taskSVCsFn func() (influxdb.TaskService, influxdb.OrganizationService, error)

func newTaskSVCs() (influxdb.TaskService, influxdb.OrganizationService, error) {
	httpClient, err := newHTTPClient()
	if err != nil {
		return nil, nil, err
	}

	orgSvc := &tenant.OrgClientService{Client: httpClient}
	return &http.TaskService{Client: httpClient}, orgSvc, nil
}

func cmdTask(f *globalFlags, opt genericCLIOpts) *cobra.Command {
	builder := newCmdTaskBuilder(newTaskSVCs, f, opt)
	return builder.cmd()
}

type cmdTaskBuilder struct {
	opts        genericCLIOpts
	globalFlags *globalFlags

	svcFn taskSVCsFn

	taskID         string
	runID          string
	taskPrintFlags taskPrintFlags
	// todo: fields of these flags structs could be pulled out for a more streamlined builder struct
	taskCreateFlags      taskCreateFlags
	taskFindFlags        taskFindFlags
	taskRerunFailedFlags taskRerunFailedFlags
	taskUpdateFlags      taskUpdateFlags
	taskRunFindFlags     taskRunFindFlags
	name                 string
	description          string
	org                  organization
	query                string
}

func newCmdTaskBuilder(svcsFn taskSVCsFn, f *globalFlags, opts genericCLIOpts) *cmdTaskBuilder {
	return &cmdTaskBuilder{
		globalFlags: f,
		opts:        opts,
		svcFn:       svcsFn,
	}
}

func (t *cmdTaskBuilder) cmd() *cobra.Command {
	cmd := t.newCmd("task", nil)
	cmd.Short = "Task management commands"
	cmd.TraverseChildren = true
	cmd.Run = seeHelp
	cmd.AddCommand(
		t.taskLogCmd(),
		t.taskRunCmd(),
		t.taskCreateCmd(),
		t.taskDeleteCmd(),
		t.taskFindCmd(),
		t.taskUpdateCmd(),
		t.taskRerunFailedCmd(),
	)

	return cmd
}

func (t *cmdTaskBuilder) newCmd(use string, runE func(*cobra.Command, []string) error) *cobra.Command {
	cmd := t.opts.newCmd(use, runE, true)
	t.globalFlags.registerFlags(t.opts.viper, cmd)
	return cmd
}

type taskPrintFlags struct {
	json        bool
	hideHeaders bool
}

type taskCreateFlags struct {
	file string
}

func (t *cmdTaskBuilder) taskCreateCmd() *cobra.Command {
	cmd := t.newCmd("create [script literal or -f /path/to/script.flux]", t.taskCreateF)
	cmd.Args = cobra.MaximumNArgs(1)
	cmd.Short = "Create task"
	cmd.Long = `Create a task with a Flux script provided via the first argument or a file or stdin`

	t.globalFlags.registerFlags(t.opts.viper, cmd)
	cmd.Flags().StringVarP(&t.taskCreateFlags.file, "file", "f", "", "Path to Flux script file")
	t.org.register(t.opts.viper, cmd, false)
	registerPrintOptions(t.opts.viper, cmd, &t.taskPrintFlags.hideHeaders, &t.taskPrintFlags.json)

	return cmd
}

func (t *cmdTaskBuilder) taskCreateF(cmd *cobra.Command, args []string) error {
	if err := t.org.validOrgFlags(&flags); err != nil {
		return err
	}

	client, err := newHTTPClient()
	if err != nil {
		return err
	}

	s := &http.TaskService{
		Client: client,
	}

	flux, err := readFluxQuery(args, t.taskCreateFlags.file)
	if err != nil {
		return fmt.Errorf("error parsing flux script: %s", err)
	}

	tc := influxdb.TaskCreate{
		Flux:         flux,
		Organization: t.org.name,
	}
	if t.org.id != "" || t.org.name != "" {
		svc, err := newOrganizationService()
		if err != nil {
			return nil
		}
		oid, err := t.org.getID(svc)
		if err != nil {
			return fmt.Errorf("error parsing organization ID: %s", err)
		}
		tc.OrganizationID = oid
	}

	tsk, err := s.CreateTask(context.Background(), tc)
	if err != nil {
		return err
	}

	return printTasks(
		cmd.OutOrStdout(),
		taskPrintOpts{
			hideHeaders: t.taskPrintFlags.hideHeaders,
			json:        t.taskPrintFlags.json,
			task:        tsk,
		},
	)
}

type taskFindFlags struct {
	user    string
	limit   int
	headers bool
}

func (t *cmdTaskBuilder) taskFindCmd() *cobra.Command {
	cmd := t.opts.newCmd("list", t.taskFindF, true)
	cmd.Short = "List tasks"
	cmd.Aliases = []string{"find", "ls"}

	t.org.register(t.opts.viper, cmd, false)
	t.globalFlags.registerFlags(t.opts.viper, cmd)
	registerPrintOptions(t.opts.viper, cmd, &t.taskPrintFlags.hideHeaders, &t.taskPrintFlags.json)
	cmd.Flags().StringVarP(&t.taskID, "id", "i", "", "task ID")
	cmd.Flags().StringVarP(&t.taskFindFlags.user, "user-id", "n", "", "task owner ID")
	cmd.Flags().IntVarP(&t.taskFindFlags.limit, "limit", "", influxdb.TaskDefaultPageSize, "the number of tasks to find")
	cmd.Flags().BoolVar(&t.taskFindFlags.headers, "headers", true, "To print the table headers; defaults true")

	return cmd
}

func (t *cmdTaskBuilder) taskFindF(cmd *cobra.Command, args []string) error {

	if err := t.org.validOrgFlags(&flags); err != nil {
		return err
	}

	client, err := newHTTPClient()
	if err != nil {
		return err
	}

	s := &http.TaskService{
		Client: client,
	}

	filter := influxdb.TaskFilter{}
	if t.taskFindFlags.user != "" {
		id, err := influxdb.IDFromString(t.taskFindFlags.user)
		if err != nil {
			return err
		}
		filter.User = id
	}

	if t.org.name != "" {
		filter.Organization = t.org.name
	}
	if t.org.id != "" {
		id, err := influxdb.IDFromString(t.org.id)
		if err != nil {
			return err
		}
		filter.OrganizationID = id
	}

	if t.taskFindFlags.limit < 1 || t.taskFindFlags.limit > influxdb.TaskMaxPageSize {
		return fmt.Errorf("limit must be between 1 and %d", influxdb.TaskMaxPageSize)
	}
	filter.Limit = t.taskFindFlags.limit

	var tasks []*influxdb.Task

	if t.taskID != "" {
		id, err := influxdb.IDFromString(t.taskID)
		if err != nil {
			return err
		}

		task, err := s.FindTaskByID(context.Background(), *id)
		if err != nil {
			return err
		}

		tasks = append(tasks, task)
	} else {
		tasks, _, err = s.FindTasks(context.Background(), filter)
		if err != nil {
			return err
		}
	}

	return printTasks(
		cmd.OutOrStdout(),
		taskPrintOpts{
			hideHeaders: t.taskPrintFlags.hideHeaders,
			json:        t.taskPrintFlags.json,
			tasks:       tasks,
		},
	)
}

type taskRerunFailedFlags struct {
	before string
	after  string
}

func (t *cmdTaskBuilder) taskRerunFailedCmd() *cobra.Command {
	cmd := t.opts.newCmd("rerun_failed", t.taskRerunFailedF, true)
	cmd.Short = "Find and Rerun failed runs/tasks"
	cmd.Aliases = []string{"rrf"}

	t.org.register(t.opts.viper, cmd, false)
	t.globalFlags.registerFlags(t.opts.viper, cmd)
	registerPrintOptions(t.opts.viper, cmd, &t.taskPrintFlags.hideHeaders, &t.taskPrintFlags.json)
	cmd.Flags().StringVarP(&t.taskID, "id", "i", "", "task ID")
	cmd.Flags().StringVarP(&t.taskRerunFailedFlags.before, "before", "bf", "", "before interval")
	cmd.Flags().StringVarP(&t.taskRerunFailedFlags.after, "after", "af", "", "after interval")

	return cmd
}

func (t *cmdTaskBuilder) taskRerunFailedF(*cobra.Command, []string) error {
	if err := t.org.validOrgFlags(&flags); err != nil {
		return err
	}

	client, err := newHTTPClient()
	if err != nil {
		return err
	}

	s := &http.TaskService{
		Client: client,
	}

	taskIDPresent := t.taskID == ""

	/*
		If no TaskID is given, use TaskFilter to get all Tasks, search for failed runs in each, then re run failed
		If TaskID given, use RunFilter to search for failed runs then re run
	*/
	taskFilter := influxdb.TaskFilter{}
	runFilter := influxdb.RunFilter{}
	if !taskIDPresent {
		if t.org.name != "" {
			taskFilter.Organization = t.org.name
		}
		if t.org.id != "" {
			orgID, err := influxdb.IDFromString(t.org.id)
			if err != nil {
				return err
			}
			taskFilter.OrganizationID = orgID
		}
	} else {
		id, err := influxdb.IDFromString(t.taskID)
		if err != nil {
			return err
		}
		runFilter.Task = *id
	}

	runFilter.BeforeTime = t.taskRerunFailedFlags.before
	runFilter.AfterTime = t.taskRerunFailedFlags.after

	var allRuns []*influxdb.Run
	if !taskIDPresent {
		allTasks, _, err := s.FindTasks(context.Background(), taskFilter)
		if err != nil {
			return err
		}

		for _, t := range allTasks {
			runFilter.Task = t.ID
			runsPerTask, _, err := s.FindRuns(context.Background(), runFilter)
			if err != nil {
				return err
			}
			allRuns = append(allRuns, runsPerTask...)
		}
	} else {
		allRuns, _, err = s.FindRuns(context.Background(), runFilter)
	}
	var failedRuns []*influxdb.Run
	for _, run := range allRuns {
		if run.Status == "failed" {
			failedRuns = append(failedRuns, run)
		}
	}
	for _, run := range failedRuns {
		newRun, err := s.RetryRun(context.Background(), run.TaskID, run.ID)
		if err != nil {
			return err
		}
		fmt.Printf("Retry for task %s's run %s queued as run %s.\n", run.TaskID, run.ID, newRun.ID)

	}
	return nil

}

type taskUpdateFlags struct {
	status string
	file   string
}

func (t *cmdTaskBuilder) taskUpdateCmd() *cobra.Command {
	cmd := t.opts.newCmd("update", t.taskUpdateF, true)
	cmd.Short = "Update task"
	cmd.Long = `Update task status or script. Provide a Flux script via the first argument or a file. Use '-' argument to read from stdin.`

	t.globalFlags.registerFlags(t.opts.viper, cmd)
	registerPrintOptions(t.opts.viper, cmd, &t.taskPrintFlags.hideHeaders, &t.taskPrintFlags.json)
	cmd.Flags().StringVarP(&t.taskID, "id", "i", "", "task ID (required)")
	cmd.Flags().StringVarP(&t.taskUpdateFlags.status, "status", "", "", "update task status")
	cmd.Flags().StringVarP(&t.taskUpdateFlags.file, "file", "f", "", "Path to Flux script file")
	cmd.MarkFlagRequired("id")

	return cmd
}

func (t *cmdTaskBuilder) taskUpdateF(cmd *cobra.Command, args []string) error {
	client, err := newHTTPClient()
	if err != nil {
		return err
	}

	s := &http.TaskService{
		Client: client,
	}

	var id influxdb.ID
	if err := id.DecodeFromString(t.taskID); err != nil {
		return err
	}

	var update influxdb.TaskUpdate
	if t.taskUpdateFlags.status != "" {
		update.Status = &t.taskUpdateFlags.status
	}

	// update flux script only if first arg or file is supplied
	if (len(args) > 0 && len(args[0]) > 0) || len(t.taskUpdateFlags.file) > 0 {
		flux, err := readFluxQuery(args, t.taskUpdateFlags.file)
		if err != nil {
			return fmt.Errorf("error parsing flux script: %s", err)
		}
		update.Flux = &flux
	}

	tsk, err := s.UpdateTask(context.Background(), id, update)
	if err != nil {
		return err
	}

	return printTasks(
		cmd.OutOrStdout(),
		taskPrintOpts{
			hideHeaders: t.taskPrintFlags.hideHeaders,
			json:        t.taskPrintFlags.json,
			task:        tsk,
		},
	)
}

func (t *cmdTaskBuilder) taskDeleteCmd() *cobra.Command {
	cmd := t.opts.newCmd("delete", t.taskDeleteF, true)
	cmd.Short = "Delete task"

	t.globalFlags.registerFlags(t.opts.viper, cmd)
	registerPrintOptions(t.opts.viper, cmd, &t.taskPrintFlags.hideHeaders, &t.taskPrintFlags.json)
	cmd.Flags().StringVarP(&t.taskID, "id", "i", "", "task id (required)")
	cmd.MarkFlagRequired("id")

	return cmd
}

func (t *cmdTaskBuilder) taskDeleteF(cmd *cobra.Command, args []string) error {
	client, err := newHTTPClient()
	if err != nil {
		return err
	}

	s := &http.TaskService{
		Client: client,
	}

	var id influxdb.ID
	err = id.DecodeFromString(t.taskID)
	if err != nil {
		return err
	}

	ctx := context.TODO()
	tsk, err := s.FindTaskByID(ctx, id)
	if err != nil {
		return err
	}

	if err = s.DeleteTask(ctx, id); err != nil {
		return err
	}

	return printTasks(
		cmd.OutOrStdout(),
		taskPrintOpts{
			hideHeaders: t.taskPrintFlags.hideHeaders,
			json:        t.taskPrintFlags.json,
			task:        tsk,
		},
	)

}

type taskPrintOpts struct {
	hideHeaders bool
	json        bool
	task        *influxdb.Task
	tasks       []*influxdb.Task
}

func printTasks(w io.Writer, opts taskPrintOpts) error {
	if opts.json {
		var v interface{} = opts.tasks
		if opts.task != nil {
			v = opts.task
		}
		return writeJSON(w, v)
	}
	tabW := internal.NewTabWriter(os.Stdout)
	defer tabW.Flush()

	tabW.HideHeaders(opts.hideHeaders)

	tabW.WriteHeaders(
		"ID",
		"Name",
		"Organization ID",
		"Organization",
		"Status",
		"Every",
		"Cron",
	)

	if opts.task != nil {
		opts.tasks = append(opts.tasks, opts.task)
	}

	for _, t := range opts.tasks {
		tabW.Write(map[string]interface{}{
			"ID":              t.ID.String(),
			"Name":            t.Name,
			"Organization ID": t.OrganizationID.String(),
			"Organization":    t.Organization,
			"Status":          t.Status,
			"Every":           t.Every,
			"Cron":            t.Cron,
		})
	}

	return nil
}

func (t *cmdTaskBuilder) taskLogCmd() *cobra.Command {
	cmd := t.opts.newCmd("log", nil, false)
	cmd.Run = seeHelp
	cmd.Short = "Log related commands"
	cmd.AddCommand(
		t.taskLogFindCmd(),
	)

	return cmd
}

func (t *cmdTaskBuilder) taskLogFindCmd() *cobra.Command {
	cmd := t.opts.newCmd("list", t.taskLogFindF, true)
	cmd.Short = "List logs for task"
	cmd.Aliases = []string{"find", "ls"}

	t.globalFlags.registerFlags(t.opts.viper, cmd)
	registerPrintOptions(t.opts.viper, cmd, &t.taskPrintFlags.hideHeaders, &t.taskPrintFlags.json)
	cmd.Flags().StringVarP(&t.taskID, "task-id", "", "", "task id (required)")
	cmd.Flags().StringVarP(&t.runID, "run-id", "", "", "run id")
	cmd.MarkFlagRequired("task-id")

	return cmd
}

func (t *cmdTaskBuilder) taskLogFindF(cmd *cobra.Command, args []string) error {
	client, err := newHTTPClient()
	if err != nil {
		return err
	}

	s := &http.TaskService{
		Client: client,
	}

	var filter influxdb.LogFilter
	id, err := influxdb.IDFromString(t.taskID)
	if err != nil {
		return err
	}
	filter.Task = *id

	if t.runID != "" {
		id, err := influxdb.IDFromString(t.runID)
		if err != nil {
			return err
		}
		filter.Run = id
	}

	ctx := context.TODO()
	logs, _, err := s.FindLogs(ctx, filter)
	if err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	if t.taskPrintFlags.json {
		return writeJSON(w, logs)
	}

	tabW := internal.NewTabWriter(w)
	defer tabW.Flush()

	tabW.HideHeaders(t.taskPrintFlags.hideHeaders)

	tabW.WriteHeaders("RunID", "Time", "Message")
	for _, log := range logs {
		tabW.Write(map[string]interface{}{
			"RunID":   log.RunID,
			"Time":    log.Time,
			"Message": log.Message,
		})
	}

	return nil
}

func (t *cmdTaskBuilder) taskRunCmd() *cobra.Command {
	cmd := t.opts.newCmd("run", nil, false)
	cmd.Run = seeHelp
	cmd.Short = "List runs for a task"
	cmd.AddCommand(
		t.taskRunFindCmd(),
		t.taskRunRetryCmd(),
	)

	return cmd
}

type taskRunFindFlags struct {
	afterTime  string
	beforeTime string
	limit      int
}

func (t *cmdTaskBuilder) taskRunFindCmd() *cobra.Command {
	cmd := t.opts.newCmd("list", t.taskRunFindF, true)
	cmd.Short = "List runs for a task"
	cmd.Aliases = []string{"find", "ls"}

	t.globalFlags.registerFlags(t.opts.viper, cmd)
	registerPrintOptions(t.opts.viper, cmd, &t.taskPrintFlags.hideHeaders, &t.taskPrintFlags.json)
	cmd.Flags().StringVarP(&t.taskID, "task-id", "", "", "task id (required)")
	cmd.Flags().StringVarP(&t.runID, "run-id", "", "", "run id")
	cmd.Flags().StringVarP(&t.taskRunFindFlags.afterTime, "after", "", "", "after time for filtering")
	cmd.Flags().StringVarP(&t.taskRunFindFlags.beforeTime, "before", "", "", "before time for filtering")
	cmd.Flags().IntVarP(&t.taskRunFindFlags.limit, "limit", "", 100, "limit the results; default is 100")

	cmd.MarkFlagRequired("task-id")

	return cmd
}

func (t *cmdTaskBuilder) taskRunFindF(cmd *cobra.Command, args []string) error {
	client, err := newHTTPClient()
	if err != nil {
		return err
	}

	s := &http.TaskService{
		Client: client,
	}

	filter := influxdb.RunFilter{
		Limit:      t.taskRunFindFlags.limit,
		AfterTime:  t.taskRunFindFlags.afterTime,
		BeforeTime: t.taskRunFindFlags.beforeTime,
	}
	taskID, err := influxdb.IDFromString(t.taskID)
	if err != nil {
		return err
	}
	filter.Task = *taskID

	var runs []*influxdb.Run
	if t.runID != "" {
		id, err := influxdb.IDFromString(t.runID)
		if err != nil {
			return err
		}
		run, err := s.FindRunByID(context.Background(), filter.Task, *id)
		if err != nil {
			return err
		}
		runs = append(runs, run)
	} else {
		runs, _, err = s.FindRuns(context.Background(), filter)
		if err != nil {
			return err
		}
	}

	w := cmd.OutOrStdout()
	if t.taskPrintFlags.json {
		if runs == nil {
			// guarantee we never return a null value from CLI
			runs = make([]*influxdb.Run, 0)
		}
		return writeJSON(w, runs)
	}

	tabW := internal.NewTabWriter(w)
	defer tabW.Flush()

	tabW.HideHeaders(t.taskPrintFlags.hideHeaders)

	tabW.WriteHeaders(
		"ID",
		"TaskID",
		"Status",
		"ScheduledFor",
		"StartedAt",
		"FinishedAt",
		"RequestedAt",
	)

	for _, r := range runs {
		scheduledFor := r.ScheduledFor.Format(time.RFC3339)
		startedAt := r.StartedAt.Format(time.RFC3339Nano)
		finishedAt := r.FinishedAt.Format(time.RFC3339Nano)
		requestedAt := r.RequestedAt.Format(time.RFC3339Nano)

		tabW.Write(map[string]interface{}{
			"ID":           r.ID,
			"TaskID":       r.TaskID,
			"Status":       r.Status,
			"ScheduledFor": scheduledFor,
			"StartedAt":    startedAt,
			"FinishedAt":   finishedAt,
			"RequestedAt":  requestedAt,
		})
	}

	return nil
}

var runRetryFlags struct {
	taskID, runID string
}

func (t *cmdTaskBuilder) taskRunRetryCmd() *cobra.Command {
	cmd := t.opts.newCmd("retry", t.runRetryF, true)
	cmd.Short = "retry a run"

	t.globalFlags.registerFlags(t.opts.viper, cmd)
	cmd.Flags().StringVarP(&runRetryFlags.taskID, "task-id", "i", "", "task id (required)")
	cmd.Flags().StringVarP(&runRetryFlags.runID, "run-id", "r", "", "run id (required)")
	cmd.MarkFlagRequired("task-id")
	cmd.MarkFlagRequired("run-id")

	return cmd
}

func (t *cmdTaskBuilder) runRetryF(*cobra.Command, []string) error {
	client, err := newHTTPClient()
	if err != nil {
		return err
	}

	s := &http.TaskService{
		Client: client,
	}

	var taskID, runID influxdb.ID
	if err := taskID.DecodeFromString(runRetryFlags.taskID); err != nil {
		return err
	}
	if err := runID.DecodeFromString(runRetryFlags.runID); err != nil {
		return err
	}

	ctx := context.TODO()
	newRun, err := s.RetryRun(ctx, taskID, runID)
	if err != nil {
		return err
	}

	fmt.Printf("Retry for task %s's run %s queued as run %s.\n", taskID, runID, newRun.ID)

	return nil
}
