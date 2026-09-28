package prssection

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"

	tea "charm.land/bubbletea/v2"
	"charm.land/log/v2"
	"github.com/gen2brain/beeep"

	"github.com/dlvhdr/gh-dash/v4/internal/tui/components/tasks"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/constants"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/context"
)

func (m *Model) watchChecks() tea.Cmd {
	pr := m.GetCurrRow()
	if pr == nil {
		return nil
	}

	prNumber := pr.GetNumber()
	title := pr.GetTitle()
	repoNameWithOwner := pr.GetRepoNameWithOwner()
	taskId := fmt.Sprintf("pr_reopen_%d", prNumber)
	task := context.Task{
		Id:           taskId,
		StartText:    fmt.Sprintf("Watching checks for PR #%d", prNumber),
		FinishedText: fmt.Sprintf("Watching checks for PR #%d", prNumber),
		State:        context.TaskStart,
		Error:        nil,
	}
	startCmd := m.Ctx.StartTask(task)
	return tea.Batch(startCmd, func() tea.Msg {
		c := exec.Command(
			"gh",
			"pr",
			"checks",
			"--watch",
			"--fail-fast",
			fmt.Sprint(prNumber),
			"-R",
			repoNameWithOwner,
		)

		var outb, errb bytes.Buffer
		c.Stdout = &outb
		c.Stderr = &errb

		err := c.Start()
		go func() {
			err := c.Wait()
			if err != nil {
				log.Error("Error waiting for watch command to finish", "err", err,
					"stderr", errb.String(), "stdout", outb.String())
			}

			checksRollup := fetchChecksOutcome(prNumber, repoNameWithOwner)
			err = beeep.Notify(
				fmt.Sprintf("gh-dash: %s", title),
				fmt.Sprintf("PR #%d in %s\n%s", prNumber, repoNameWithOwner, checksRollup),
				"",
			)
			if err != nil {
				log.Error("Error showing system notification", "err", err)
			}
		}()

		return constants.TaskFinishedMsg{
			SectionId:   m.Id,
			SectionType: SectionType,
			TaskId:      taskId,
			Err:         err,
			Msg: tasks.UpdatePRMsg{
				PrNumber: prNumber,
			},
		}
	})
}

// fetchChecksOutcome asks GitHub for the checks' final state once the watch ends.
// The row data captured when watching started is stale by then.
func fetchChecksOutcome(prNumber int, repoNameWithOwner string) string {
	// gh exits non-zero for failing or pending checks but still prints the JSON.
	out, _ := exec.Command(
		"gh",
		"pr",
		"checks",
		fmt.Sprint(prNumber),
		"-R",
		repoNameWithOwner,
		"--json",
		"bucket",
	).Output()

	var checks []struct{ Bucket string }
	if err := json.Unmarshal(out, &checks); err != nil {
		log.Error("Error reading checks result", "err", err, "output", string(out))
		return "Couldn't read the checks result"
	}
	buckets := make([]string, 0, len(checks))
	for _, check := range checks {
		buckets = append(buckets, check.Bucket)
	}
	return summarizeChecks(buckets)
}

func summarizeChecks(buckets []string) string {
	if len(buckets) == 0 {
		return "No checks reported"
	}
	isPending := false
	for _, bucket := range buckets {
		switch bucket {
		case "fail", "cancel":
			return "❌ Checks have failed"
		case "pending":
			isPending = true
		}
	}
	if isPending {
		return " Checks are pending"
	}
	return "✅ Checks have passed"
}
