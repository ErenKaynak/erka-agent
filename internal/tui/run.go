package tui

import (
	"context"
	"errors"
	"io"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

func Run(model *Model, input io.Reader, output io.Writer) error {
	_, err := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output)).Run()
	return err
}

var ErrCredentialEntryCancelled = errors.New("credential entry cancelled")

type credentialModel struct {
	ctx       context.Context
	input     textinput.Model
	provider  string
	secret    string
	cancelled bool
}

func newCredentialModel(ctx context.Context, provider string) *credentialModel {
	input := textinput.New()
	input.Prompt = "API key: "
	input.Placeholder = "paste credential"
	input.EchoMode = textinput.EchoPassword
	input.EchoCharacter = '•'
	input.CharLimit = 16 << 10
	input.Focus()
	return &credentialModel{ctx: ctx, input: input, provider: Sanitize(provider)}
}

func (model *credentialModel) Init() tea.Cmd { return model.input.Focus() }
func (model *credentialModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "enter":
			if strings.TrimSpace(model.input.Value()) != "" {
				model.secret = model.input.Value()
				return model, tea.Quit
			}
		case "esc", "ctrl+c":
			model.cancelled = true
			return model, tea.Quit
		}
	}
	var command tea.Cmd
	model.input, command = model.input.Update(message)
	return model, command
}
func (model *credentialModel) View() tea.View {
	view := tea.NewView("erka needs a " + model.provider + " credential.\nIt will be saved in your OS credential store when available.\n\n" + model.input.View() + "\n\nEnter save · Esc cancel")
	view.AltScreen = true
	view.WindowTitle = "erka — credential"
	return view
}

func PromptCredential(ctx context.Context, input io.Reader, output io.Writer, provider string) (string, error) {
	model := newCredentialModel(ctx, provider)
	result, err := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return "", err
	}
	final, ok := result.(*credentialModel)
	if !ok || final.cancelled || final.secret == "" {
		return "", ErrCredentialEntryCancelled
	}
	return final.secret, nil
}

var _ tea.Model = (*credentialModel)(nil)
