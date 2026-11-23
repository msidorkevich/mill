package runner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// inputKeys are the tool input fields that best describe a call, in order of preference:
// Bash command, file path, search pattern, then the rest.
var inputKeys = []string{"command", "file_path", "notebook_path", "pattern", "path", "url", "query", "description", "prompt"}

// LastAction reads a claude stream-json file and describes the agent's latest action: the
// last tool call as "<Tool> <short input>", or the last text the agent wrote when that is its
// latest content. It returns "" when the file holds no action yet. Lines that do not parse,
// such as a partially written last line, are skipped.
func LastAction(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read agent stream: %w", err)
	}
	defer f.Close()
	last := ""
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		if a := actionOf(line); a != "" {
			last = a
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("read agent stream %s: %w", path, rerr)
		}
	}
	return last, nil
}

// actionOf describes the last tool call or text block of an assistant event line.
func actionOf(line []byte) string {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant" {
		return ""
	}
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(ev.Message.Content, &blocks) != nil {
		return ""
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		switch b.Type {
		case "tool_use":
			return oneLine(strings.TrimSpace(b.Name + " " + toolInput(b.Input)))
		case "text":
			if t := oneLine(b.Text); t != "" {
				return t
			}
		}
	}
	return ""
}

func toolInput(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range inputKeys {
		if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// oneLine collapses all whitespace runs, so the result fits on one terminal line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Actions reads a claude stream-json file and describes every action in it, oldest first, in
// the format LastAction uses: one line per assistant event, for its last tool call or text.
// Lines that do not parse, such as a partially written last line, are skipped.
func Actions(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read agent stream: %w", err)
	}
	defer f.Close()
	var out []string
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		if a := actionOf(line); a != "" {
			out = append(out, a)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read agent stream %s: %w", path, rerr)
		}
	}
	return out, nil
}
