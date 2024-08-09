// Library to interact with ansi/v100 style terminals.
package terminal // import "fortio.org/terminal"

import (
	"bufio"
	"errors"
	"io"
	"os"
	"slices"

	"fortio.org/log"
	"fortio.org/term"
)

type Terminal struct {
	fd          int
	oldState    *term.State
	term        *term.Terminal
	Out         io.Writer
	historyFile string
	capacity    int
}

// Open opens stdin as a terminal, do `defer terminal.Close()`
// to restore the terminal to its original state upon exit.
func Open() (*Terminal, error) {
	rw := struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stderr}
	t := &Terminal{
		fd: int(os.Stdin.Fd()),
	}
	t.term = term.NewTerminal(rw, "")
	t.Out = t.term
	if !t.IsTerminal() {
		t.Out = os.Stderr // no need to add \r for non raw mode.
		return t, nil
	}
	var err error
	t.oldState, err = term.MakeRaw(t.fd)
	if err != nil {
		return nil, err
	}
	t.term.SetBracketedPasteMode(true) // Seems useful to have it on by default.
	// 100 here matches the default value in term. should expose it there.
	t.capacity = 100 - 1 // -1 to fit "pending" command.
	return t, nil
}

func (t *Terminal) IsTerminal() bool {
	return term.IsTerminal(t.fd)
}

// Setups fortio logger to write to the terminal as needed to preserve prompt.
func (t *Terminal) LoggerSetup() {
	isTerm := t.IsTerminal()
	// t.Out will add the needed \r for each \n when term is in raw mode
	log.SetOutput(t.Out)
	log.Config.ForceColor = isTerm
	log.SetColorMode()
}

func (t *Terminal) SetHistoryFile(f string) error {
	if f == "" {
		log.Infof("No history file specified")
		return nil
	}
	if t.capacity <= 0 {
		log.Infof("No history capacity set, ignoring history file %s", f)
		return nil
	}
	if !t.IsTerminal() {
		log.Infof("Not a terminal, not setting history file")
		return nil
	}
	t.historyFile = f
	entries, err := readOrCreateHistory(f)
	if err != nil {
		t.historyFile = "" // so we don't try to save during defer'ed close if we can't read
		return err
	}
	start := 0
	if len(entries) > t.capacity {
		log.Infof("History file %s has more than %d entries, truncating.", f, t.capacity)
		start = len(entries) - t.capacity
	} else {
		log.Infof("Loaded %d history entries from %s", len(entries), f)
	}
	for _, e := range entries[start:] {
		t.term.AddToHistory(e)
	}
	return nil
}

// Forward the term history API and not just the high level file history api above.

// AddToHistory add commands to the history.
func (t *Terminal) AddToHistory(commands ...string) {
	t.term.AddToHistory(commands...)
}

// History returns the current history state.
func (t *Terminal) History() []string {
	return t.term.History()
}

// NewHistory creates/resets the history to a new one with the given capacity.
// need + 1 to fit "pending" command.
func (t *Terminal) NewHistory(capacity int) {
	if capacity < 0 {
		log.Errf("Invalid history capacity %d, ignoring", capacity)
		return
	}
	t.capacity = capacity
	t.term.NewHistory(capacity + 1)
}

// AutoHistory enables/disables auto history (default is enabled).
func (t *Terminal) AutoHistory(enabled bool) {
	t.term.AutoHistory(enabled)
}

func readOrCreateHistory(f string) ([]string, error) {
	// open file or create it
	h, err := os.OpenFile(f, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		log.Errf("Error opening history file %s: %v", f, err)
		return nil, err
	}
	defer h.Close()
	// read lines separated by \n
	var lines []string
	scanner := bufio.NewScanner(h)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Errf("Error reading history file %s: %v", f, err)
		return nil, err
	}
	return lines, nil
}

func saveHistory(f string, h []string) {
	// open file or create it
	hf, err := os.OpenFile(f, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		log.Errf("Error opening history file %s: %v", f, err)
		return
	}
	defer hf.Close()
	// write lines separated by \n
	for _, l := range h {
		_, err := hf.WriteString(l + "\n")
		if err != nil {
			log.Errf("Error writing history file %s: %v", f, err)
			return
		}
	}
}

func (t *Terminal) Close() error {
	if t.oldState == nil {
		return nil
	}
	// To avoid prompt being repeated on the last line (shouldn't be necessary but... is
	// consider fixing in term instead)
	t.term.SetPrompt("") // will still reprint the last command on ^C in middle of typing.
	err := term.Restore(t.fd, t.oldState)
	t.oldState = nil
	t.Out = os.Stderr
	// saving history if any
	if t.historyFile == "" || t.capacity <= 0 {
		log.Debugf("No history file %q or capacity %d, not saving history", t.historyFile, t.capacity)
		return nil
	}
	h := t.term.History()
	log.LogVf("got history %v", h)
	slices.Reverse(h)
	extra := len(h) - t.capacity
	if extra > 0 {
		h = h[extra:] // truncate to max capacity otherwise extra ones will get out of order
	}
	log.Infof("Saving history (%d commands) to %s", len(h), t.historyFile)
	saveHistory(t.historyFile, h)
	return err
}

func (t *Terminal) ReadLine() (string, error) {
	c, err := t.term.ReadLine()
	// That error isn't an error that needs to be propagated,
	// it's just to allow copy/paste without autocomplete.
	if errors.Is(err, term.ErrPasteIndicator) {
		return c, nil
	}
	return c, err
}

func (t *Terminal) SetPrompt(s string) {
	t.term.SetPrompt(s)
}

// Pass "this" back so AutoCompleteCallback can use t.Out etc.
// (compared to the original x/term callback).
type AutoCompleteCallback func(t *Terminal, line string, pos int, key rune) (newLine string, newPos int, ok bool)

func (t *Terminal) SetAutoCompleteCallback(f AutoCompleteCallback) {
	t.term.AutoCompleteCallback = func(line string, pos int, key rune) (newLine string, newPos int, ok bool) {
		return f(t, line, pos, key)
	}
}
