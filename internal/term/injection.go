//go:build darwin || linux

package term

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

func failedLine(err error) string {
	message := strings.TrimPrefix(err.Error(), "llm: ")
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		message = "the model did not answer"
	}
	return "# " + message
}

func (k *keyboard) recordAnswer(q *query, command string) {
	question := ""
	if !q.keptQuestion {
		question = strings.TrimSpace(q.submitted)
		if question == "" {
			question = strings.TrimSpace(q.editor.question())
		}
		q.keptQuestion = true
	}
	tool := q.editor.agentMode() && !strings.HasPrefix(strings.TrimSpace(command), "#")
	k.state.capture.answered(question, command, tool)
}

// injection is the pure revision state for one streamed shell line.
type injection struct {
	typed []rune
	text  string
	// Later bytes can complete an invalid UTF-8 suffix, changing an earlier
	// replacement rune. Only complete prefixes can take the append path.
	validUTF8 bool
}

func (k *keyboard) revise(q *query, inject *injection, command string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if q.editor.finished() {
		return errTakenBack
	}
	unchecked := command
	appending := (inject.validUTF8 || len(inject.typed) == 0) && strings.HasPrefix(command, inject.text)
	if appending {
		unchecked = command[len(inject.text):]
	}
	if !typable(unchecked) {
		return fmt.Errorf("%w: %q", errUntypable, command)
	}
	validUTF8 := utf8.ValidString(unchecked)
	var next []rune
	var erase []byte
	var insert string
	if appending {
		next = append(inject.typed, []rune(unchecked)...)
		insert = unchecked
		if !validUTF8 {
			insert = string(next[len(inject.typed):])
		}
	} else {
		next = []rune(command)
		erase, insert = revision(inject.typed, next)
	}
	if len(erase) == 0 && insert == "" {
		return nil
	}
	// The shell can leave paste mode while the model is responding. Without
	// bracketed paste, a newline would execute the command instead of editing it.
	if strings.ContainsRune(insert, '\n') && !k.state.pasteMode() {
		return fmt.Errorf("%w: %q", errUntypable, command)
	}
	if err := q.erase(); err != nil {
		return err
	}
	inject.typed, inject.text, inject.validUTF8 = next, command, validUTF8
	if err := k.toShell(erase); err != nil {
		return err
	}
	return k.insert(insert)
}

func (k *keyboard) resetInjection(inject *injection) {
	k.mu.Lock()
	*inject = injection{}
	k.mu.Unlock()
}

func (k *keyboard) rethink(q *query, thought string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if q.editor.finished() {
		return errTakenBack
	}
	runes := []rune(thought)
	for index, char := range runes {
		if char < space || char == del || char >= 0x80 && char < 0xa0 {
			runes[index] = ' '
		}
	}
	if extra := len(runes) - thoughtLimit; extra > 0 {
		runes = slices.Clone(runes[extra:])
	}
	q.thought = runes
	return nil
}

// revision preserves the common prefix, then erases and replaces the suffix.
// Erasure stays separate: backspaces inside a paste would become literal text.
func revision(typed, command []rune) (erase []byte, insert string) {
	kept := 0
	for kept < min(len(typed), len(command)) && typed[kept] == command[kept] {
		kept++
	}
	erase = make([]byte, len(typed)-kept)
	for i := range erase {
		erase[i] = del
	}
	return erase, string(command[kept:])
}

// typable rejects editing controls. Newlines are allowed only because insert
// sends them inside a bracketed paste.
func typable(command string) bool {
	return strings.IndexFunc(command, func(char rune) bool {
		return (char < space && char != '\n') || char == del
	}) < 0
}

const (
	// Bracketed paste makes newlines editable text rather than Enter presses.
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// insert types a single line or sends a complete bracketed paste for multiline
// text. Both brackets travel in one write so subsequent keys cannot enter it.
func (k *keyboard) insert(text string) error {
	if text == "" {
		return nil
	}
	if !strings.ContainsRune(text, '\n') {
		return k.toShell([]byte(text))
	}

	k.state.pasted(text)
	keys := make([]byte, 0, len(pasteStart)+len(text)+len(pasteEnd))
	keys = append(keys, pasteStart...)
	keys = append(keys, text...)
	keys = append(keys, pasteEnd...)
	n, err := k.master.Write(keys)
	if err != nil {
		// Close a partially written paste before returning keyboard ownership.
		if n >= len(pasteStart) && n < len(keys) {
			_, _ = k.master.Write([]byte(pasteEnd))
		}
		return fmt.Errorf("term: pasting at the shell: %w", err)
	}
	return nil
}
