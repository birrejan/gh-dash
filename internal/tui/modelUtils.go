package tui

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"text/template"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	log "charm.land/log/v2"

	"github.com/dlvhdr/gh-dash/v4/internal/config"
	"github.com/dlvhdr/gh-dash/v4/internal/data"
	"github.com/dlvhdr/gh-dash/v4/internal/shell"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/common"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/components/notificationrow"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/components/prrow"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/components/section"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/constants"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/context"
	"github.com/dlvhdr/gh-dash/v4/internal/tui/markdown"
)

func (m *Model) getCurrSection() section.Section {
	sections := m.getCurrentViewSections()
	if len(sections) == 0 || m.currSectionId >= len(sections) {
		return nil
	}
	return sections[m.currSectionId]
}

func (m *Model) getCurrRowData() data.RowData {
	section := m.getCurrSection()
	if section == nil {
		return nil
	}
	return section.GetCurrRow()
}

func (m *Model) getSectionAt(id int) section.Section {
	sections := m.getCurrentViewSections()
	if len(sections) <= id {
		return nil
	}
	return sections[id]
}

func (m *Model) getPrevSectionId() int {
	return max(0, (m.currSectionId - 1))
}

func (m *Model) getNextSectionId() int {
	return min((m.currSectionId + 1), len(m.getCurrentViewSections())-1)
}

type IssueCommandTemplateInput struct {
	RepoName    string
	RepoPath    string
	IssueNumber int
	HeadRefName string
}

func (m *Model) executeKeybinding(key string) tea.Cmd {
	currRowData := m.getCurrRowData()

	for _, keybinding := range m.ctx.Config.Keybindings.Universal {
		if keybinding.Key != key {
			continue
		}

		log.Info("executing keybind", "key", keybinding.Key, "command", keybinding.Command)
		return m.runCustomUniversalCommand(keybinding)
	}

	switch m.ctx.View {
	case config.IssuesView:
		for _, keybinding := range m.ctx.Config.Keybindings.Issues {
			if keybinding.Key != key {
				continue
			}

			switch data := currRowData.(type) {
			case *data.IssueData:
				return m.runCustomIssueCommand(keybinding, data)
			}
		}
	case config.PRsView:
		for _, keybinding := range m.ctx.Config.Keybindings.Prs {
			if keybinding.Key != key || keybinding.Command == "" {
				continue
			}

			log.Debug("executing keybind", "key", keybinding.Key, "command", keybinding.Command)

			switch data := currRowData.(type) {
			case *prrow.Data:
				return m.runCustomPRCommand(keybinding, data)
			}
		}
	case config.RepoView:
		for _, keybinding := range m.ctx.Config.Keybindings.Branches {
			if keybinding.Key != key || keybinding.Command == "" {
				continue
			}

			log.Debug("executing keybind", "key", keybinding.Key, "command", keybinding.Command)

			switch data := currRowData.(type) {
			case *prrow.Data:
				return m.runCustomBranchCommand(keybinding, data)
			}
		}
	case config.NotificationsView:
		for _, keybinding := range m.ctx.Config.Keybindings.Notifications {
			if keybinding.Key != key || keybinding.Command == "" {
				continue
			}
			log.Debug(
				"executing notification keybind",
				"key",
				keybinding.Key,
				"command",
				keybinding.Command,
			)
			if nData, ok := currRowData.(*notificationrow.Data); ok {
				return m.runCustomNotificationCommand(keybinding, nData)
			}
		}

		if nData, ok := currRowData.(*notificationrow.Data); ok {
			switch nData.Notification.Subject.Type {
			case "PullRequest":
				for _, keybinding := range m.ctx.Config.Keybindings.Prs {
					if keybinding.Key != key || keybinding.Command == "" {
						continue
					}
					log.Debug(
						"executing notification PR keybind",
						"key",
						keybinding.Key,
						"command",
						keybinding.Command,
					)
					return m.runCustomNotificationPRCommand(keybinding, nData)
				}
			case "Issue":
				for _, keybinding := range m.ctx.Config.Keybindings.Issues {
					if keybinding.Key != key || keybinding.Command == "" {
						continue
					}
					log.Debug(
						"executing notification issue keybind",
						"key",
						keybinding.Key,
						"command",
						keybinding.Command,
					)
					return m.runCustomNotificationIssueCommand(keybinding, nData)
				}
			}
		}
	default:
		// Not a valid case - ignore it
	}

	return nil
}

// resolveTemplateInput builds the input map for a keybinding command template.
// It merges context-specific data and resolves RepoPath via the repoPaths config mapping.
// ctxRepoPath is the path of the repo gh-dash was started from (may be empty).
func resolveTemplateInput(
	contextData *map[string]any,
	repoPaths map[string]string,
	ctxRepoPath string,
) map[string]any {
	// A generic map is a pretty easy & flexible way to populate a template if there's no pressing need
	// for structured data, existing structs, etc. Especially if holes in the data are expected.
	// Common data shared across contexts could be set here.
	input := map[string]any{}

	// Merge data specific to the context the command is being run in onto any common data, overwriting duplicate keys.
	if contextData != nil {
		maps.Copy(input, *contextData)
	}

	// Append in the local RepoPath only if it can be found
	if input["RepoName"] != nil {
		if repoPath, ok := common.GetRepoLocalPath(
			input["RepoName"].(string),
			repoPaths,
		); ok {
			input["RepoPath"] = repoPath
		}
	}

	// Fall back to the current repo path if RepoPath was not resolved via repoPaths config
	if input["RepoPath"] == nil && ctxRepoPath != "" {
		input["RepoPath"] = ctxRepoPath
	}

	return input
}

// runCustomCommand executes a user-defined command.
// keybinding.Command is a template string that will be parsed with the input data.
// contextData is a map of key-value pairs of data specific to the context the command is being run in.
func (m *Model) runCustomCommand(keybinding config.Keybinding, contextData *map[string]any) tea.Cmd {
	commandTemplate := keybinding.Command
	input := resolveTemplateInput(contextData, m.ctx.Config.RepoPaths, m.ctx.RepoPath)

	cmd, err := template.New("keybinding_command").Parse(commandTemplate)
	if err != nil {
		log.Fatal("Failed parse keybinding template", "error", err)
	}

	// Set the command to error out if required input (e.g. RepoPath) is missing
	cmd = cmd.Option("missingkey=error")

	var buff bytes.Buffer
	err = cmd.Execute(&buff, input)
	if err != nil {
		return func() tea.Msg {
			log.Error("failed to parsetemplate", "err", err, "commandTemplate", commandTemplate)
			return constants.ErrMsg{Err: fmt.Errorf("failed to parsetemplate %s", commandTemplate)}
		}
	}
	return m.executeCommand(keybinding, buff.String())
}

func (m *Model) runCustomPRCommand(keybinding config.Keybinding, prData *prrow.Data) tea.Cmd {
	return m.runCustomCommand(keybinding,
		&map[string]any{
			"RepoName":    prData.GetRepoNameWithOwner(),
			"PrNumber":    prData.Primary.Number,
			"HeadRefName": prData.Primary.HeadRefName,
			"BaseRefName": prData.Primary.BaseRefName,
			"Author":      prData.Primary.Author.Login,
		})
}

func (m *Model) runCustomIssueCommand(keybinding config.Keybinding, issueData *data.IssueData) tea.Cmd {
	return m.runCustomCommand(keybinding,
		&map[string]any{
			"RepoName":    issueData.GetRepoNameWithOwner(),
			"IssueNumber": issueData.Number,
			"IssueTitle":  issueData.Title,
			"Author":      issueData.Author.Login,
		},
	)
}

func (m *Model) runCustomBranchCommand(keybinding config.Keybinding, branchData *prrow.Data) tea.Cmd {
	if reflect.ValueOf(branchData).IsNil() {
		return m.executeCommand(keybinding, keybinding.Command)
	}
	input := map[string]any{
		"RepoPath": m.ctx.RepoPath,
	}
	if branchData != nil {
		maps.Copy(input,
			map[string]any{
				"RepoName":    branchData.GetRepoNameWithOwner(),
				"PrNumber":    branchData.Primary.Number,
				"HeadRefName": branchData.Primary.HeadRefName,
				"BaseRefName": branchData.Primary.BaseRefName,
				"Author":      branchData.Primary.Author.Login,
			})
	}
	return m.runCustomCommand(keybinding, &input)
}

func (m *Model) runCustomUniversalCommand(keybinding config.Keybinding) tea.Cmd {
	input := map[string]any{"RepoPath": m.ctx.RepoPath}
	return m.runCustomCommand(keybinding, &input)
}

func (m *Model) runCustomNotificationPRCommand(
	keybinding config.Keybinding,
	nData *notificationrow.Data,
) tea.Cmd {
	fields := map[string]any{
		"RepoName": nData.GetRepoNameWithOwner(),
		"PrNumber": nData.GetNumber(),
	}
	if pr := m.notificationView.GetSubjectPR(); pr != nil {
		fields["HeadRefName"] = pr.Primary.HeadRefName
		fields["BaseRefName"] = pr.Primary.BaseRefName
		fields["Author"] = pr.Primary.Author.Login
	}
	return m.runCustomCommand(keybinding, &fields)
}

func (m *Model) runCustomNotificationIssueCommand(
	keybinding config.Keybinding,
	nData *notificationrow.Data,
) tea.Cmd {
	fields := map[string]any{
		"RepoName":    nData.GetRepoNameWithOwner(),
		"IssueNumber": nData.GetNumber(),
	}
	if issue := m.notificationView.GetSubjectIssue(); issue != nil {
		fields["Author"] = issue.Author.Login
	}
	return m.runCustomCommand(keybinding, &fields)
}

func (m *Model) runCustomNotificationCommand(
	keybinding config.Keybinding,
	nData *notificationrow.Data,
) tea.Cmd {
	fields := map[string]any{
		"RepoName": nData.GetRepoNameWithOwner(),
		"Number":   nData.GetNumber(),
	}
	return m.runCustomCommand(keybinding, &fields)
}

type execProcessFinishedMsg struct{}

func (m *Model) executeCommand(keybinding config.Keybinding, cmd string) tea.Cmd {
	if keybinding.Background {
		return m.executeBackgroundCommand(keybinding, cmd)
	}
	return m.executeCustomCommand(cmd)
}

// executeBackgroundCommand runs cmd without handing it the terminal, so the
// dashboard stays on screen; progress and errors show up as a task in the footer.
func (m *Model) executeBackgroundCommand(keybinding config.Keybinding, cmd string) tea.Cmd {
	log.Debug("executing custom command in the background", "cmd", cmd)
	text := keybinding.Name
	if text == "" {
		text = cmd
	}
	id := fmt.Sprintf("custom_command_%d", time.Now().UnixNano())
	startCmd := m.ctx.StartTask(context.Task{
		Id:           id,
		StartText:    text,
		FinishedText: text,
		State:        context.TaskStart,
	})

	return tea.Batch(startCmd, func() tea.Msg {
		out, err := shell.Command(cmd).CombinedOutput()
		if output := strings.TrimSpace(string(out)); err != nil && output != "" {
			err = fmt.Errorf("%w: %s", err, output)
		}
		return constants.TaskFinishedMsg{TaskId: id, Err: err}
	})
}

func (m *Model) executeCustomCommand(cmd string) tea.Cmd {
	log.Debug("executing custom command", "cmd", cmd)
	c := shell.Command(cmd)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			mdRenderer := markdown.GetMarkdownRenderer(m.ctx.ScreenWidth, m.ctx)
			md, mdErr := mdRenderer.Render(fmt.Sprintf("While running: `%s`", cmd))
			if mdErr != nil {
				return constants.ErrMsg{Err: mdErr}
			}
			return constants.ErrMsg{Err: errors.New(
				lipgloss.JoinVertical(lipgloss.Left,
					fmt.Sprintf("Whoops, got an error: %s", err),
					md,
				),
			)}
		}
		return execProcessFinishedMsg{}
	})
}

func (m *Model) notify(text string) tea.Cmd {
	id := fmt.Sprint(time.Now().Unix())
	startCmd := m.ctx.StartTask(
		context.Task{
			Id:           id,
			StartText:    text,
			FinishedText: text,
			State:        context.TaskStart,
		})

	finishCmd := func() tea.Msg {
		return constants.TaskFinishedMsg{
			TaskId: id,
		}
	}

	return tea.Sequence(startCmd, finishCmd)
}

func (m *Model) notifyErr(text string) tea.Cmd {
	id := fmt.Sprint(time.Now().Unix())
	startCmd := m.ctx.StartTask(
		context.Task{
			Id:           id,
			StartText:    text,
			FinishedText: text,
			State:        context.TaskStart,
		})

	finishCmd := func() tea.Msg {
		return constants.TaskFinishedMsg{
			TaskId: id,
			Err:    errors.New(text),
		}
	}

	return tea.Sequence(startCmd, finishCmd)
}
