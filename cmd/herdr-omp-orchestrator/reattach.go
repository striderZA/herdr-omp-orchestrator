package main

// Issue #18: re-establishing a lost primary is an explicit action the user
// invokes and confirms. Herdr runs action commands without a terminal, so the
// action (`open-reattach`) only opens the plugin pane the manifest declares, and
// that pane (`reattach`) is where the user reads the plan and confirms it.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/task"
)

// reattachEntrypoint is the pane id herdr-plugin.toml declares for the
// confirmation pane.
const reattachEntrypoint = "reattach"

// Environment the reattach routes use. envFocusedPane is the pane Herdr injects
// for the invocation; envReattachPane carries it from the action into the pane it
// opens, because that pane's own focus is the overlay, not the replacement.
const (
	envFocusedPane  = "HERDR_PANE_ID"
	envReattachPane = "HERDR_ORCH_REATTACH_PANE"
)

const (
	// reattachPlanTimeout bounds the verification the user is shown. The wait for
	// the answer itself is the reader's and is not bounded.
	reattachPlanTimeout = 30 * time.Second
	// reattachTimeout bounds what follows the confirmation: the write, one notice
	// to the replacement session and the wait for its extension's handshake.
	reattachTimeout = 2 * time.Minute
)

// runOpenReattach is the `[[actions]]` route: it opens the confirmation pane for
// the pane the action was invoked from, the replacement the user means.
func runOpenReattach(env map[string]string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(env[envStateDir]) == "" {
		return emit(stdout, stderr, failureBody("state_dir_unavailable", envStateDir+" is not set"))
	}
	binary := herdrBinary(env)
	if binary == "" {
		return emit(stdout, stderr, failureBody("herdr_unavailable", envHerdrBinary+" is not set"))
	}
	replacement := firstNonEmpty(invocationContext(env).FocusedPaneID, env[envFocusedPane])
	if replacement == "" {
		return emit(stdout, stderr, failureBody("context_unavailable",
			"the invocation names no pane to reattach; run the action from the pane that runs the replacement OMP"))
	}
	client, err := herdr.NewClient(binary)
	if err != nil {
		return emit(stdout, stderr, failureBody("herdr_unavailable", err.Error()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), openBoardTimeout)
	defer cancel()
	opened, err := client.Call(ctx, "plugin", "pane", "open",
		"--plugin", pluginID, "--entrypoint", reattachEntrypoint,
		"--env", envReattachPane+"="+replacement)
	if err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	return emit(stdout, stderr, map[string]any{"ok": true, "replacement": replacement, "pane": opened})
}

// primaryReattacher is the coordinator's reattach surface. The route confirms
// with the user between planning and reattaching; it decides nothing itself.
type primaryReattacher interface {
	PaneProject(ctx context.Context, paneID string) (string, error)
	PlanReattach(ctx context.Context, projectKey, paneID string) (coord.ReattachPlan, error)
	Reattach(ctx context.Context, projectKey, paneID string, confirmed coord.ReattachPlan) (task.Project, error)
}

// runReattach is the `[[panes]]` route. It shows what the reattach would record,
// asks for confirmation and, only on an explicit yes, asks the coordinator to
// record and confirm it. Human text goes to stdout, where the user is reading;
// the machine-readable envelope every other route writes goes to stderr, so the
// plugin log still carries a stable code.
func runReattach(env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	paneID := strings.TrimSpace(env[envReattachPane])
	if paneID == "" {
		return finishReattach(stdout, stderr, bufio.NewReader(stdin),
			failureBody("bad_request", envReattachPane+" is not set: open this pane with the reattach action"))
	}
	coordinator, closeCoordinator, err := openCoordinator(env)
	if err != nil {
		return finishReattach(stdout, stderr, bufio.NewReader(stdin), failureBody(coordCode(err), err.Error()))
	}
	defer closeCoordinator()
	in := bufio.NewReader(stdin)
	return finishReattach(stdout, stderr, in, reattachSession(context.Background(), coordinator, paneID, in, stdout))
}

// reattachSession plans, asks and acts. It returns the envelope describing the
// outcome; a declined confirmation is a successful answer that changed nothing.
func reattachSession(ctx context.Context, coordinator primaryReattacher, paneID string, in *bufio.Reader, stdout io.Writer) map[string]any {
	planCtx, cancel := context.WithTimeout(ctx, reattachPlanTimeout)
	key, err := coordinator.PaneProject(planCtx, paneID)
	var plan coord.ReattachPlan
	if err == nil {
		plan, err = coordinator.PlanReattach(planCtx, key, paneID)
	}
	cancel()
	if err != nil {
		return failureBody(coordCode(err), err.Error())
	}

	fmt.Fprint(stdout, plan.Describe())
	fmt.Fprint(stdout, "\nType yes to reattach, anything else to cancel: ")
	answer, _ := in.ReadString('\n')
	if reply := strings.ToLower(strings.TrimSpace(answer)); reply != "yes" && reply != "y" {
		fmt.Fprint(stdout, "\nCancelled. Nothing was changed.\n")
		return map[string]any{"ok": true, "reattached": false, "project": key, "pane": paneID}
	}

	actCtx, cancel := context.WithTimeout(ctx, reattachTimeout)
	defer cancel()
	fmt.Fprint(stdout, "\nReattaching...\n")
	project, err := coordinator.Reattach(actCtx, key, paneID, plan)
	if err != nil {
		return failureBody(coordCode(err), err.Error())
	}
	fmt.Fprintf(stdout, "\nReattached and confirmed: pane %s is now the primary of %s.\nWorkers, tasks and the inbox were kept.\n",
		project.LaunchPaneID, project.Key)
	return map[string]any{"ok": true, "reattached": true, "project": project.Key, "pane": project.LaunchPaneID, "session": project.OMPSessionID}
}

// finishReattach prints a failure where the user can read it, writes the envelope
// and holds the overlay open until the user is done reading, because the pane
// closes as soon as this process exits.
func finishReattach(stdout, stderr io.Writer, in *bufio.Reader, body map[string]any) int {
	if ok, _ := body["ok"].(bool); !ok {
		code, message := "internal", ""
		if failure, found := body["error"].(map[string]any); found {
			code, _ = failure["code"].(string)
			message, _ = failure["message"].(string)
		}
		fmt.Fprintf(stdout, "\nNot reattached (%s): %s\n", code, message)
		if code == "reattach_pending" {
			fmt.Fprint(stdout, "The replacement is recorded but unconfirmed; the board marks it until it is confirmed.\n")
		}
	}
	fmt.Fprint(stdout, "\nPress Enter to close.")
	_, _ = in.ReadString('\n')
	fmt.Fprintln(stdout)
	return emit(stderr, stderr, body)
}
