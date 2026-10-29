//go:build windows

package main

// mpv.go — integration with the bundled mpv player.
//
// Transport
// ---------
// mpv runs as a separate process and Kouki talks to it over mpv's own JSON IPC
// channel, exposed as a Windows named pipe (`input-ipc-server`).
//
// libmpv bindings (e.g. github.com/gen2brain/go-mpv) were evaluated and rejected:
//
//   - They need a `libmpv-2.dll` next to the binary. The bundled mpv.exe is the
//     user's own shinchiro build, which ships *no* libmpv DLL; the only sources
//     are third-party `mpv-dev-*.7z` archives of a *different* mpv build. Adding
//     one would contradict bundling the user's MPV with their presets.
//   - libmpv is LGPLv2.1+ while the bundled mpv.exe is GPLv2+. Linking would force
//     the whole application under GPLv2+, a licence change the project cannot make
//     silently on the user's behalf.
//   - Embedding video also needs a native child HWND. That conflicts with a
//     WebView2 (DirectComposition) surface, which does not composite foreign
//     child windows over the web content.
//
// The JSON IPC channel is the officially supported integration surface for an
// out-of-process player, works with both the bundled and any user-supplied mpv,
// and needs no C toolchain. See README.md for the full rationale.
//
// Fixes over the previous implementation
// --------------------------------------
//   - Correct response demultiplexing. Asynchronous mpv events carry no
//     `request_id`; the old code accepted `request_id == 0` as a valid reply to
//     any outstanding request, so a single event could desynchronise the whole
//     channel. Events are now queued instead of consumed.
//   - A single mutex serialises access, so the poll loop and the GUI can share
//     one connection safely.
//   - Real error classification instead of substring matching on error text.
//   - Kouki now owns the mpv process lifecycle (launch with correct geometry,
//     graceful quit through IPC, terminate as a fallback).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// wire format
// ---------------------------------------------------------------------------

type mpvRequest struct {
	Command   []any `json:"command"`
	RequestID int64 `json:"request_id,omitempty"`
}

type mpvResponse struct {
	// Event is set for asynchronous notifications, which carry no request_id.
	Event     string          `json:"event,omitempty"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data,omitempty"`
	RequestID int64           `json:"request_id,omitempty"`
}

// errNoFilePlaying is the sentinel used by queryMpvState to distinguish "mpv is
// alive but nothing is loaded" from "the connection died".
var errNoFilePlaying = errors.New("no file playing")

// ---------------------------------------------------------------------------
// connection
// ---------------------------------------------------------------------------

// mpvConn is a synchronous client for one mpv JSON IPC connection.
//
// Requests are serialised by mu. This keeps the 250 ms poll loop and the GUI's
// own 500 ms refresh from interleaving on the wire, which the previous
// implementation did not guarantee.
type mpvConn struct {
	conn net.Conn
	dec  *json.Decoder

	mu     sync.Mutex
	nextID int64

	// events collected while waiting for a reply. Bounded so a chatty mpv
	// cannot grow it without limit.
	events []mpvResponse

	closed bool
}

// dialMpv opens the mpv IPC pipe, retrying until the deadline expires.
//
// mpv creates the pipe at startup, so a short retry window makes the common
// "user just launched mpv" case connect immediately instead of waiting for the
// outer reconnect delay.
//
// Each failed attempt used to append its own line, and with a player that is
// not running the poll produced an identical "arquivo especificado não
// encontrado" line two or three times a second — 244 of them in a few minutes,
// which buried the launch details that actually explain a failure. The missing
// pipe is now reported once per transition, and the quiet state is cleared on
// every successful connection, so the log shows the moment the channel appeared
// or stopped appearing and nothing in between.
var mpvDialState struct {
	mu       sync.Mutex
	reported string
}

// logDialOnce records a connection failure the first time it is seen for a pipe.
func logDialOnce(pipe, msg string) {
	mpvDialState.mu.Lock()
	defer mpvDialState.mu.Unlock()
	if mpvDialState.reported == pipe {
		return
	}
	mpvDialState.reported = pipe
	logAppend(msg)
}

// logDialReset clears the quiet state once the channel answers.
func logDialReset(pipe string) {
	mpvDialState.mu.Lock()
	mpvDialState.reported = ""
	mpvDialState.mu.Unlock()
}

func dialMpv(pipe string, timeout time.Duration) (*mpvConn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error

	// A pipe that does not exist is the expected idle state, so it is reported
	// once and then left alone; anything else is always worth a line.
	//
	// Classified by error value, never by message text: the text is localised
	// by Windows, so matching a Portuguese sentence only ever worked on a
	// Portuguese system.
	notFound := func(err error) bool {
		return err != nil && (errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND))
	}

	for {
		// A nil timeout makes DialPipe wait forever on a busy pipe, which would
		// freeze the poll loop on a wedged mpv. Bound each attempt instead; the
		// outer loop owns the overall deadline.
		attemptTimeout := 300 * time.Millisecond
		conn, err := winio.DialPipe(pipe, &attemptTimeout)
		if err == nil {
			m := &mpvConn{conn: conn, dec: json.NewDecoder(conn)}
			if err := m.ping(); err != nil {
				logAppend("mpv: pipe ", pipe, " respondeu mal: ", err.Error())
				_ = conn.Close()
				lastErr = err
			} else {
				logDialReset(pipe)
				logAppend("mpv: conectado a ", pipe)
				return m, nil
			}
		} else {
			if notFound(err) {
				logDialOnce(pipe, "mpv: sem canal IPC em "+pipe+" — aguardando o player")
			} else {
				logDialOnce(pipe, "mpv: falha ao conectar em "+pipe+": "+err.Error())
			}
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("mpv indisponível")
			}
			return nil, lastErr
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// ping verifies the peer really speaks mpv IPC before we trust the connection.
func (m *mpvConn) ping() error {
	_, err := m.request("get_property", "mpv-version")
	return err
}

// request sends one command and returns its decoded data.
//
// Replies are matched strictly on request_id. Anything else that arrives while
// waiting (an event) is queued for drainEvents.
func (m *mpvConn) request(cmd ...any) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, errors.New("conexão mpv encerrada")
	}

	rid := atomic.AddInt64(&m.nextID, 1)
	payload, err := json.Marshal(mpvRequest{Command: cmd, RequestID: rid})
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')

	if err := m.conn.SetWriteDeadline(time.Now().Add(mpvRPCDeadline)); err != nil {
		return nil, err
	}
	if _, err := m.conn.Write(payload); err != nil {
		return nil, err
	}
	if err := m.conn.SetReadDeadline(time.Now().Add(mpvRPCDeadline)); err != nil {
		return nil, err
	}

	for {
		var resp mpvResponse
		if err := m.dec.Decode(&resp); err != nil {
			return nil, err
		}
		// mpv always sets "error": "success" on success and omits request_id on
		// events, so a mismatch here means "not our reply".
		if resp.RequestID != rid {
			m.pushEvent(resp)
			continue
		}
		if resp.Error != "" && resp.Error != "success" {
			return nil, fmt.Errorf("mpv: %s", resp.Error)
		}
		return resp.Data, nil
	}
}

const maxQueuedEvents = 256

func (m *mpvConn) pushEvent(ev mpvResponse) {
	if len(m.events) >= maxQueuedEvents {
		return
	}
	m.events = append(m.events, ev)
}

// drainEvents returns and clears the asynchronous notifications collected while
// waiting for replies. Used to detect mpv shutting itself down.
func (m *mpvConn) drainEvents() []mpvResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.events) == 0 {
		return nil
	}
	out := m.events
	m.events = nil
	return out
}

// setCommand is fire-and-forget: it writes a command without waiting for the
// reply, for cases where the answer carries no information we need.
func (m *mpvConn) setCommand(cmd ...any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("conexão mpv encerrada")
	}
	payload, err := json.Marshal(mpvRequest{Command: cmd})
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := m.conn.SetWriteDeadline(time.Now().Add(mpvRPCDeadline)); err != nil {
		return err
	}
	_, err = m.conn.Write(payload)
	return err
}

func (m *mpvConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	return m.conn.Close()
}

// Alive reports whether mpv still answers on this connection.
func (m *mpvConn) Alive() bool { return m.ping() == nil }

// ---------------------------------------------------------------------------
// property decoding
// ---------------------------------------------------------------------------

// rawString decodes a JSON string property.
func rawString(d json.RawMessage) (string, bool) {
	if len(d) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(d, &s); err != nil {
		return "", false
	}
	return s, true
}

// rawFloat decodes a JSON number property.
func rawFloat(d json.RawMessage) (float64, bool) {
	if len(d) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(d, &f); err != nil {
		return 0, false
	}
	return f, true
}

// rawBool decodes a JSON boolean property.
func rawBool(d json.RawMessage) (bool, bool) {
	if len(d) == 0 {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(d, &b); err != nil {
		return false, false
	}
	return b, true
}

// queryMpvState reads the playback state Kouki cares about.
//
// Individual property failures are tolerated (mpv omits properties that do not
// apply, e.g. duration for a live stream) so a partial state is still useful;
// only "nothing is loaded at all" is reported as an error.
func queryMpvState(m *mpvConn) (mpvState, error) {
	if m == nil {
		return mpvState{}, errors.New("sem conexão mpv")
	}
	var st mpvState

	if d, err := m.request("get_property", "path"); err == nil {
		if s, ok := rawString(d); ok && s != "" {
			st.Path = extractStreamFilename(s)
		}
	}
	if d, err := m.request("get_property", "filename"); err == nil {
		if s, ok := rawString(d); ok && s != "" {
			st.FileName = extractStreamFilename(s)
		}
	}
	if d, err := m.request("get_property", "media-title"); err == nil {
		if s, ok := rawString(d); ok {
			st.MediaTitle = s
		}
	}
	if d, err := m.request("get_property", "duration"); err == nil {
		if f, ok := rawFloat(d); ok && f > 0 {
			st.Duration = f
		}
	}
	if d, err := m.request("get_property", "time-pos"); err == nil {
		if f, ok := rawFloat(d); ok && f >= 0 {
			st.TimePos = f
		}
	}
	if d, err := m.request("get_property", "pause"); err == nil {
		if b, ok := rawBool(d); ok {
			st.Pause = b
		}
	}
	if d, err := m.request("get_property", "eof-reached"); err == nil {
		if b, ok := rawBool(d); ok {
			st.EOFReached = b
		}
	}

	if st.FileName == "" && st.MediaTitle == "" {
		return st, errNoFilePlaying
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// shared connection
// ---------------------------------------------------------------------------

// mpvHub publishes the live mpv connection so the GUI can read playback state
// and issue transport commands without opening a second pipe.
//
// The poll loop owns the connection's lifecycle; the GUI only borrows it.
var sharedMpvConn = &mpvHolder{updates: make(chan struct{}, 1)}

type mpvHolder struct {
	mu      sync.RWMutex
	conn    *mpvConn
	updates chan struct{}
	// lastErr is why the most recent connection attempt failed. The GUI reports
	// it verbatim instead of claiming the player did not open the channel, which
	// is a guess — it could equally have been a wrong pipe name, a player that
	// had not created it yet, or a single-instance clash.
	lastErr string

	// snap is the playback state the poll loop read most recently. The window
	// reads this copy instead of talking to mpv itself: a Snapshot that went over
	// the pipe cost seven synchronous round trips, and the repaint path asked for
	// it three times per tick on walk's own thread, so a slow or busy mpv froze
	// the whole window.
	snap   mpvState
	snapOK bool
	snapAt time.Time
}

// mpvSnapshotMaxAge is how long a published state stays believable. The poll
// loop can be busy for a while (a rate-limited AniList lookup sleeps up to a
// minute), so this is generous: the window keeps showing "connected" through
// that, and only the playback details are dropped.
const mpvSnapshotMaxAge = 15 * time.Second

// publish stores the state the poll loop has just read. ok is false when mpv is
// connected but has no file loaded.
func (h *mpvHolder) publish(st mpvState, ok bool) {
	h.mu.Lock()
	h.snap, h.snapOK, h.snapAt = st, ok, time.Now()
	h.mu.Unlock()
}

// Connected reports whether an IPC connection is currently published.
func (h *mpvHolder) Connected() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conn != nil
}

// setErr records why the last dial failed.
func (h *mpvHolder) setErr(err error) {
	h.mu.Lock()
	if err != nil {
		h.lastErr = err.Error()
	} else {
		h.lastErr = ""
	}
	h.mu.Unlock()
}

// LastError returns the last connection failure, for diagnostics.
func (h *mpvHolder) LastError() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastErr
}

func (h *mpvHolder) set(c *mpvConn) {
	h.mu.Lock()
	h.conn = c
	if c != nil {
		h.lastErr = ""
	}
	// A new (or dropped) connection invalidates whatever was published for the
	// previous one.
	h.snap, h.snapOK, h.snapAt = mpvState{}, false, time.Time{}
	h.mu.Unlock()
	// Coalesced wake-up: one pending notification is enough, the GUI re-reads
	// the full state anyway.
	select {
	case h.updates <- struct{}{}:
	default:
	}
}

func (h *mpvHolder) get() *mpvConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conn
}

// Changed returns a channel that receives when the connection is replaced.
func (h *mpvHolder) Changed() <-chan struct{} { return h.updates }

// Snapshot returns the playback state the poll loop last published, if mpv is
// connected and has a file loaded. It never touches the pipe, so it is safe to
// call from the repaint path.
func (h *mpvHolder) Snapshot() (mpvState, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.conn == nil || !h.snapOK || time.Since(h.snapAt) > mpvSnapshotMaxAge {
		return mpvState{}, false
	}
	return h.snap, true
}

// ---------------------------------------------------------------------------
// error classification
// ---------------------------------------------------------------------------

// isPipeGone reports whether err means "mpv went away", as opposed to a
// transient protocol or timeout error.
//
// The previous implementation matched on English substrings of the error text,
// which silently broke whenever the message changed. This checks the concrete
// error values Windows and Go produce for a dead pipe instead.
func isPipeGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrClosed) || errors.Is(err, windows.ERROR_BROKEN_PIPE) {
		return true
	}
	if errors.Is(err, winio.ErrTimeout) {
		// A pipe that exists but never answers means mpv is frozen or exiting.
		return true
	}
	// windows.ERROR_* are syscall.Errno constants (identical numeric values),
	// so they compare directly against the wrapped error.
	var werr syscall.Errno
	if errors.As(err, &werr) {
		switch werr {
		case windows.ERROR_BROKEN_PIPE,
			windows.ERROR_PIPE_NOT_CONNECTED,
			windows.ERROR_NO_DATA,
			windows.ERROR_INVALID_HANDLE,
			windows.ERROR_FILE_NOT_FOUND,
			windows.ERROR_PIPE_BUSY:
			return true
		}
	}
	// A decode error means the peer closed mid-stream.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// player lifecycle
// ---------------------------------------------------------------------------

// mpvLaunchOptions describes how Kouki should start the player.
type mpvLaunchOptions struct {
	// Pipe is the IPC pipe the child must expose (no \\.\pipe\ prefix).
	Pipe string
	// Files are the media files to load. Empty starts an idle player.
	Files []string
}

// mpvPlayer owns an mpv process started by Kouki.
//
// Kouki launches the player instead of requiring the user to start it by hand.
// That is what makes the bundled mpv.conf (presets, shaders, scripts) actually
// take effect: mpv only auto-loads portable_config/ when it is the binary being
// run.
type mpvPlayer struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	pipePath string
}

// IsRunning reports whether Kouki currently owns a live mpv process.
//
// reap clears p.cmd as soon as the process is gone, so "running" is simply "p.cmd
// is set". Reading cmd.ProcessState here instead raced with cmd.Wait, which is
// the call that writes it.
func (p *mpvPlayer) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil
}

// PipePath returns the named pipe the owned player exposes.
func (p *mpvPlayer) PipePath() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pipePath
}

// Launch starts the mpv executable.
//
// The goal is that this is indistinguishable from the user double-clicking
// mpv.exe in Explorer: absolute path, working directory next to the binary, and
// nothing on the command line beyond what the player genuinely needs.
//
//	--idle=yes
//	    Without it mpv finds no media, exits immediately and returns 0 — a clean
//	    successful exit that looks exactly like a crash from the outside. This is
//	    the one flag that makes "start the player" mean "start the player".
//
//	--force-window=yes
//	    Without it there is nothing on screen. With --idle and no media loaded,
//	    mpv has no frame to decode, so it opens no video output and the process
//	    sits there headless: the IPC channel works, but "Iniciar MPV" appears to
//	    do nothing at all. --force-window is documented for exactly this case —
//	    a player that opens a window while waiting for something to play.
//
//	--input-ipc-server=<pipe>
//	    The channel Kouki reads. It is also present in the player's own mpv.conf,
//	    but passing it here means an edited config cannot silently break the
//	    integration.
//
// Nothing else is passed. --autofit and --window-maximized resized the window to
// a size the user never asked for, and --msg-level / --msg-file only affected
// logging: the player's output is captured by redirecting the process streams
// instead, which changes nothing about how mpv behaves.
func (p *mpvPlayer) Launch(exe string, opt mpvLaunchOptions) error {
	if exe == "" || !fileExists(exe) {
		return errors.New("MPV não encontrado em " + exe)
	}
	// An absolute path matters: Start inherits this process' working directory,
	// which is not the player's folder.
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}

	pipe := opt.Pipe
	if pipe == "" {
		pipe = "mpv-pipe"
	}
	pipePath := `\\.\pipe\` + pipe

	p.mu.Lock()
	if p.cmd != nil {
		p.mu.Unlock()
		return errors.New("o player já está em execução")
	}
	p.mu.Unlock()

	logFile := filepath.Join(ConfigDir(), "mpv.log")
	_ = os.MkdirAll(ConfigDir(), 0o700)
	rotateLogIfLarge(logFile, 2<<20)

	args := []string{
		"--idle=yes",
		"--force-window=yes",
		"--input-ipc-server=" + pipePath,
	}
	args = append(args, opt.Files...)

	cmd := exec.Command(exe, args...)
	// The player's own folder, which is where Explorer's double-click would also
	// start it. Note that portable_config is located next to the *executable*,
	// not relative to this directory — an earlier comment here claimed otherwise.
	cmd.Dir = filepath.Dir(exe)

	logAppend("mpv: exe=", exe)
	logAppend("mpv: dir=", cmd.Dir)
	logAppend("mpv: pipe criado=", pipePath)
	// The poll loop dials cfg.MpvPipe, which comes from loadConfig, while this
	// pipe comes from mpvPipeName. They agree by default and must agree in fact;
	// logging both makes a divergence visible instead of leaving it to be guessed.
	logAppend("mpv: pipe esperado pelo poll=", mpvPipeName())
	logAppend("mpv: args=", strings.Join(args, " "))

	// mpv.exe is a GUI binary with no console, and Go hands a child with nil
	// Stdout/Stderr to os.DevNull — so anything mpv had to say was discarded.
	// Redirecting the streams only decides where the output goes; it does not
	// change how mpv behaves.
	if lf, lerr := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); lerr == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
		defer lf.Close()
	} else {
		logAppend("mpv: log indisponível: ", lerr.Error())
	}

	if err := cmd.Start(); err != nil {
		logAppend("mpv: Start falhou: ", err.Error())
		return err
	}

	pid := cmd.Process.Pid
	p.mu.Lock()
	p.cmd = cmd
	p.pipePath = pipePath
	p.mu.Unlock()
	logAppend("mpv: pid=", strconv.Itoa(pid))

	go p.reap(cmd, pid)

	// Confirm it stayed up, so a clean early exit is reported instead of looking
	// like "nothing happened".
	go func() {
		time.Sleep(1500 * time.Millisecond)
		if !processAlive(pid) {
			logAppend("mpv: encerrou logo apos iniciar (pid ", strconv.Itoa(pid),
				") — ver ", logFile)
		}
	}()

	return nil
}

// reap waits for the player to exit and records why, if it does.
func (p *mpvPlayer) reap(cmd *exec.Cmd, pid int) {
	err := cmd.Wait()

	// The process is gone: stop reporting it as ours.
	p.mu.Lock()
	if p.cmd == cmd {
		p.cmd = nil
	}
	p.mu.Unlock()

	logFile := filepath.Join(ConfigDir(), "mpv.log")

	// If the player produced nothing, say so. An empty mpv.log means mpv never
	// got far enough to log, which points at option parsing or process startup
	// rather than at playback.
	if st, serr := os.Stat(logFile); serr != nil {
		logAppend("mpv: o player nao criou ", logFile, " — falhou antes de inicializar")
	} else if st.Size() == 0 {
		logAppend("mpv: ", logFile, " ficou vazio — o player nao escreveu nada")
	}

	if err == nil {
		logAppend("mpv: encerrou codigo 0")
		return
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	logAppend("mpv: encerrou codigo ", strconv.Itoa(code), ": ", err.Error())
	switch code {
	case 1:
		logAppend("mpv: codigo 1 = erro em tempo de execucao; detalhe em ", logFile)
	case 2:
		logAppend("mpv: codigo 2 = erro de opcao/initializacao; detalhe em ", logFile)
	case 3:
		logAppend("mpv: codigo 3 = encerrado por sinal")
	}
}

// Stop ends the owned player: ask mpv to quit through IPC, then force it.
//
// The IPC path lets mpv flush its state and close the window cleanly; the
// fallback exists because mpv may already be gone or wedged.
func (p *mpvPlayer) Stop(pipePath string, grace time.Duration) {
	if grace <= 0 {
		grace = 3 * time.Second
	}

	// Ask nicely first so mpv can save its state and close its own window.
	if pipePath != "" {
		if m, err := dialMpv(pipePath, 2*time.Second); err == nil {
			_ = m.setCommand("quit")
			_ = m.Close()
		}
	}

	p.mu.Lock()
	cmd := p.cmd
	p.cmd = nil
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}

	// Wait for a clean exit, then take it down.
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(cmd.Process.Pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
}

// stillActiveExitCode is STILL_ACTIVE from the Windows SDK (winbase.h): the
// value GetExitCodeProcess reports while the process has not exited yet.
//
// It is not exposed by golang.org/x/sys/windows, so it is declared here with the
// documented value rather than substituted with an arbitrary one.
const stillActiveExitCode = 259

// processAlive reports whether a PID is still running.
//
// The access mask comes from golang.org/x/sys/windows because Go's syscall
// package does not define PROCESS_QUERY_LIMITED_INFORMATION. It is an untyped
// constant, so it is passed straight to syscall.OpenProcess.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)

	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActiveExitCode
}

// ---------------------------------------------------------------------------
// transport commands used by the GUI
// ---------------------------------------------------------------------------

// mpvDispatch runs a transport command on a live connection.
//
// This keeps the GUI free of IPC details: it names an action and mpv.go turns it
// into the right JSON command.
func mpvDispatch(m *mpvConn, name string) error {
	if m == nil {
		return errors.New("MPV não está em execução")
	}
	var cmd []any
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "toggle-pause":
		cmd = []any{"cycle", "pause"}
	case "play":
		cmd = []any{"set_property", "pause", false}
	case "pause":
		cmd = []any{"set_property", "pause", true}
	case "stop":
		cmd = []any{"stop"}
	case "quit":
		cmd = []any{"quit"}
	case "next":
		cmd = []any{"playlist-next"}
	case "previous":
		cmd = []any{"playlist-prev"}
	case "fullscreen":
		cmd = []any{"cycle", "fullscreen"}
	case "volume-up":
		cmd = []any{"add_property", "volume", 5}
	case "volume-down":
		cmd = []any{"add_property", "volume", -5}
	default:
		return fmt.Errorf("comando mpv desconhecido: %s", name)
	}
	return m.setCommand(cmd...)
}
