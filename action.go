package radix

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/mediocregopher/radix/v3/resp"
	"github.com/mediocregopher/radix/v3/resp/resp2"
)

// Action performs a task using a Conn.
type Action interface {
	// Keys returns the keys which will be acted on. Empty slice or nil may be
	// returned if no keys are being acted on. The returned slice must not be
	// modified.
	Keys() []string

	// Perform actually performs the Action using an existing Conn.
	Perform(c Conn) error
}

// CmdAction is a sub-class of Action which can be used in two different ways.
// The first is as a normal Action, where Perform is called with a Conn and
// returns once the Action has been completed.
//
// The second way is as a Pipeline-able command, where one or more commands are
// written in one step (via the MarshalRESP method) and their results are read
// later (via the UnmarshalRESP method).
//
// When used directly with Do then MarshalRESP/UnmarshalRESP are not called, and
// when used in a Pipeline the Perform method is not called.
type CmdAction interface {
	Action
	resp.Marshaler
	resp.Unmarshaler
}

var noKeyCmds = map[string]bool{
	"SENTINEL": true,

	"CLUSTER":   true,
	"READONLY":  true,
	"READWRITE": true,
	"ASKING":    true,

	"AUTH":   true,
	"ECHO":   true,
	"PING":   true,
	"QUIT":   true,
	"SELECT": true,
	"SWAPDB": true,

	"KEYS":      true,
	"MIGRATE":   true,
	"OBJECT":    true,
	"RANDOMKEY": true,
	"WAIT":      true,
	"SCAN":      true,

	"EVAL":    true,
	"EVALSHA": true,
	"SCRIPT":  true,

	"BGREWRITEAOF": true,
	"BGSAVE":       true,
	"CLIENT":       true,
	"COMMAND":      true,
	"CONFIG":       true,
	"DBSIZE":       true,
	"DEBUG":        true,
	"FLUSHALL":     true,
	"FLUSHDB":      true,
	"INFO":         true,
	"LASTSAVE":     true,
	"MONITOR":      true,
	"ROLE":         true,
	"SAVE":         true,
	"SHUTDOWN":     true,
	"SLAVEOF":      true,
	"SLOWLOG":      true,
	"SYNC":         true,
	"TIME":         true,

	"DISCARD": true,
	"EXEC":    true,
	"MULTI":   true,
	"UNWATCH": true,
	"WATCH":   true,
}

func cmdString(m resp.Marshaler) string {
	// we go way out of the way here to display the command as it would be sent
	// to redis. This is pretty similar logic to what the stub does as well
	buf := new(bytes.Buffer)
	if err := m.MarshalRESP(buf); err != nil {
		return fmt.Sprintf("error creating string: %q", err.Error())
	}
	var ss []string
	err := resp2.RawMessage(buf.Bytes()).UnmarshalInto(resp2.Any{I: &ss})
	if err != nil {
		return fmt.Sprintf("error creating string: %q", err.Error())
	}
	for i := range ss {
		ss[i] = strconv.QuoteToASCII(ss[i])
	}
	return "[" + strings.Join(ss, " ") + "]"
}

func marshalBulkString(prevErr error, w io.Writer, str string) error {
	if prevErr != nil {
		return prevErr
	}
	return resp2.BulkString{S: str}.MarshalRESP(w)
}

func marshalBulkStringBytes(prevErr error, w io.Writer, b []byte) error {
	if prevErr != nil {
		return prevErr
	}
	return resp2.BulkStringBytes{B: b}.MarshalRESP(w)
}

////////////////////////////////////////////////////////////////////////////////

type cmdAction struct {
	rcv  interface{}
	cmd  string
	args []string

	flat     bool
	flatKey  [1]string // use array to avoid allocation in Keys
	flatArgs []interface{}
}

// BREAM: Benchmarks Rule Everything Around Me
var cmdActionPool sync.Pool

func getCmdAction() *cmdAction {
	if ci := cmdActionPool.Get(); ci != nil {
		return ci.(*cmdAction)
	}
	return new(cmdAction)
}

// Cmd is used to perform a redis command and retrieve a result. It should not
// be passed into Do more than once.
//
// If the receiver value of Cmd is a primitive, a slice/map, or a struct then a
// pointer must be passed in. It may also be an io.Writer, an
// encoding.Text/BinaryUnmarshaler, or a resp.Unmarshaler. See the package docs
// for more on how results are unmarshaled into the receiver.
func Cmd(rcv interface{}, cmd string, args ...string) CmdAction {
	c := getCmdAction()
	*c = cmdAction{
		rcv:  rcv,
		cmd:  cmd,
		args: args,
	}
	return c
}

// FlatCmd is like Cmd, but the arguments can be of almost any type, and FlatCmd
// will automatically flatten them into a single array of strings. Like Cmd, a
// FlatCmd should not be passed into Do more than once.
//
// FlatCmd does _not_ work for commands whose first parameter isn't a key, or
// (generally) for MSET. Use Cmd for those.
//
// FlatCmd supports using a resp.LenReader (an io.Reader with a Len() method) as
// an argument. *bytes.Buffer is an example of a LenReader, and the resp package
// has a NewLenReader function which can wrap an existing io.Reader.
//
// FlatCmd also supports encoding.Text/BinaryMarshalers. It does _not_ currently
// support resp.Marshaler.
//
// The receiver to FlatCmd follows the same rules as for Cmd.
func FlatCmd(rcv interface{}, cmd, key string, args ...interface{}) CmdAction {
	c := getCmdAction()
	*c = cmdAction{
		rcv:      rcv,
		cmd:      cmd,
		flat:     true,
		flatKey:  [1]string{key},
		flatArgs: args,
	}
	return c
}

func findStreamsKeys(args []string) []string {
	for i, arg := range args {
		if strings.ToUpper(arg) != "STREAMS" {
			continue
		}

		// after STREAMS only stream keys and IDs can be given and since there must be the same number of keys and ids
		// we can just take half of remaining arguments as keys. If the number of IDs does not match the number of
		// keys the command will fail later when send to Redis so no need for us to handle that case.
		ids := len(args[i+1:]) / 2

		return args[i+1 : len(args)-ids]
	}

	return nil
}

func (c *cmdAction) Keys() []string {
	if c.flat {
		return c.flatKey[:]
	}

	cmd := strings.ToUpper(c.cmd)
	if cmd == "BITOP" && len(c.args) > 1 { // antirez why you do this
		return c.args[1:]
	} else if cmd == "XINFO" {
		if len(c.args) < 2 {
			return nil
		}
		return c.args[1:2]
	} else if cmd == "XGROUP" && len(c.args) > 1 {
		return c.args[1:2]
	} else if cmd == "XREAD" || cmd == "XREADGROUP" { // antirez why you still do this
		return findStreamsKeys(c.args)
	} else if noKeyCmds[cmd] || len(c.args) == 0 {
		return nil
	}
	return c.args[:1]
}

func (c *cmdAction) flatMarshalRESP(w io.Writer) error {
	var err error
	a := resp2.Any{
		I:                     c.flatArgs,
		MarshalBulkString:     true,
		MarshalNoArrayHeaders: true,
	}
	arrL := 2 + a.NumElems()
	err = resp2.ArrayHeader{N: arrL}.MarshalRESP(w)
	err = marshalBulkString(err, w, c.cmd)
	err = marshalBulkString(err, w, c.flatKey[0])
	if err != nil {
		return err
	}
	return a.MarshalRESP(w)
}

func (c *cmdAction) MarshalRESP(w io.Writer) error {
	if c.flat {
		return c.flatMarshalRESP(w)
	}

	err := resp2.ArrayHeader{N: len(c.args) + 1}.MarshalRESP(w)
	err = marshalBulkString(err, w, c.cmd)
	for i := range c.args {
		err = marshalBulkString(err, w, c.args[i])
	}
	return err
}

func (c *cmdAction) UnmarshalRESP(br *bufio.Reader) error {
	if err := (resp2.Any{I: c.rcv}).UnmarshalRESP(br); err != nil {
		return err
	}
	cmdActionPool.Put(c)
	return nil
}

func (c *cmdAction) Perform(conn Conn) error {
	return conn.EncodeDecode(c, c)
}

func (c *cmdAction) String() string {
	return cmdString(c)
}

func (c *cmdAction) ClusterCanRetry() bool {
	return true
}

////////////////////////////////////////////////////////////////////////////////

// MaybeNil is a type which wraps a receiver. It will first detect if what's
// being received is a nil RESP type (either bulk string or array), and if so
// set Nil to true. If not the return value will be unmarshalled into Rcv
// normally. If the response being received is an empty array then the EmptyArray
// field will be set and Rcv unmarshalled into normally.
type MaybeNil struct {
	Nil        bool
	EmptyArray bool
	Rcv        interface{}
}

// UnmarshalRESP implements the method for the resp.Unmarshaler interface.
func (mn *MaybeNil) UnmarshalRESP(br *bufio.Reader) error {
	var rm resp2.RawMessage
	err := rm.UnmarshalRESP(br)
	switch {
	case err != nil:
		return err
	case rm.IsNil():
		mn.Nil = true
		return nil
	case rm.IsEmptyArray():
		mn.EmptyArray = true
		fallthrough // to not break backwards compatibility
	default:
		return rm.UnmarshalInto(resp2.Any{I: mn.Rcv})
	}
}

////////////////////////////////////////////////////////////////////////////////

// EvalScript contains the body of a script to be used with redis' EVAL
// functionality. Call Cmd on a EvalScript to actually create an Action which
// can be run.
type EvalScript struct {
	script, sum string
	numKeys     int
}

// NewEvalScript initializes a EvalScript instance. numKeys corresponds to the
// number of arguments which will be keys when Cmd is called
func NewEvalScript(numKeys int, script string) EvalScript {
	sumRaw := sha1.Sum([]byte(script))
	sum := hex.EncodeToString(sumRaw[:])
	return EvalScript{
		script:  script,
		sum:     sum,
		numKeys: numKeys,
	}
}

var (
	evalsha = []byte("EVALSHA")
	eval    = []byte("EVAL")
)

type evalAction struct {
	EvalScript
	args []string
	rcv  interface{}

	eval bool
}

// Cmd is like the top-level Cmd but it uses the the EvalScript to perform an
// EVALSHA command (and will automatically fallback to EVAL as necessary). args
// must be at least as long as the numKeys argument of NewEvalScript.
func (es EvalScript) Cmd(rcv interface{}, args ...string) Action {
	if len(args) < es.numKeys {
		panic("not enough arguments passed into EvalScript.Cmd")
	}
	return &evalAction{
		EvalScript: es,
		args:       args,
		rcv:        rcv,
	}
}

func (ec *evalAction) Keys() []string {
	return ec.args[:ec.numKeys]
}

func (ec *evalAction) MarshalRESP(w io.Writer) error {
	// EVAL(SHA) script/sum numkeys args...
	if err := (resp2.ArrayHeader{N: 3 + len(ec.args)}).MarshalRESP(w); err != nil {
		return err
	}

	var err error
	if ec.eval {
		err = marshalBulkStringBytes(err, w, eval)
		err = marshalBulkString(err, w, ec.script)
	} else {
		err = marshalBulkStringBytes(err, w, evalsha)
		err = marshalBulkString(err, w, ec.sum)
	}

	err = marshalBulkString(err, w, strconv.Itoa(ec.numKeys))
	for i := range ec.args {
		err = marshalBulkString(err, w, ec.args[i])
	}
	return err
}

func (ec *evalAction) Perform(conn Conn) error {
	run := func(eval bool) error {
		ec.eval = eval
		return conn.EncodeDecode(ec, resp2.Any{I: ec.rcv})
	}

	err := run(false)
	if err != nil && strings.HasPrefix(err.Error(), "NOSCRIPT") {
		err = run(true)
	}
	return err
}

func (ec *evalAction) ClusterCanRetry() bool {
	return true
}

////////////////////////////////////////////////////////////////////////////////

type pipelineMarshalerUnmarshaler struct {
	resp.Marshaler
	resp.Unmarshaler

	err error
}

// TODO this could use a sync.Pool probably
type pipeline struct {
	cmds []CmdAction
	mm   []pipelineMarshalerUnmarshaler

	// Conn is only set during the Perform method. It's primary purpose is to
	// provide for the methods which aren't EncodeDecode during the inner
	// Actions' Perform calls, in the unlikely event that they are needed.
	Conn
}

// Pipeline returns an Action which first writes multiple commands to a Conn in
// a single write, then reads their responses in a single read. This reduces
// network delay into a single round-trip.
//
// NOTE that, while a Pipeline performs all commands on a single Conn, it
// shouldn't be used by itself for MULTI/EXEC transactions, because if there's
// an error it won't discard the incomplete transaction. Use WithConn or
// EvalScript for transactional functionality instead.
func Pipeline(cmds ...CmdAction) Action {
	return &pipeline{
		cmds: cmds,
		mm:   make([]pipelineMarshalerUnmarshaler, 0, len(cmds)),
	}
}

var _ Conn = new(pipeline)

func (p *pipeline) Keys() []string {
	m := map[string]bool{}
	for _, cmd := range p.cmds {
		for _, k := range cmd.Keys() {
			m[k] = true
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func (p *pipeline) EncodeDecode(m resp.Marshaler, u resp.Unmarshaler) error {
	p.mm = append(p.mm, pipelineMarshalerUnmarshaler{
		Marshaler: m, Unmarshaler: u,
	})
	return nil
}

func (p *pipeline) setErr(startingAt int, err error) {
	for i := startingAt; i < len(p.mm); i++ {
		p.mm[i].err = err
	}
}

func (p *pipeline) MarshalRESP(w io.Writer) error {
	for i := range p.mm {
		if p.mm[i].Marshaler == nil {
			continue
		} else if err := p.mm[i].Marshaler.MarshalRESP(w); err == nil {
			continue
		} else {
			p.setErr(i, err)
			break
		}
	}
	return nil
}

func (p *pipeline) UnmarshalRESP(br *bufio.Reader) error {
	for i := range p.mm {
		if p.mm[i].Unmarshaler == nil || p.mm[i].err != nil {
			continue
		} else if err := p.mm[i].Unmarshaler.UnmarshalRESP(br); err == nil {
			continue
		} else if errors.As(err, new(resp.ErrDiscarded)) {
			p.mm[i].err = err
			continue
		} else {
			p.setErr(i, err)
			break
		}
	}
	return nil
}

func (p *pipeline) Perform(c Conn) error {
	p.Conn = c
	defer func() { p.Conn = nil }()

	for _, cmd := range p.cmds {
		// any errors that happen within Perform will not be IO errors, because
		// pipelineConn is suppressing all potential IO errors
		if err := cmd.Perform(p); err != nil {
			return err
		}
	}

	// pipeline's Marshal/UnmarshalRESP methods don't return an error, but
	// instead swallow any errors they come across. If EncodeDecode returns an
	// error it means something else the Conn was doing errored (like flushing
	// its write buffer). There's not much to be done except return that error
	// for all pipelineMarshalerUnmarshalers.
	if err := c.EncodeDecode(p, p); err != nil {
		p.setErr(0, err)
		return err
	}

	// look through any errors encountered, if any. Perform will only return the
	// first error encountered, but it does take into account all the others
	// when determining if that error should be wrapped in ErrDiscarded.
	//
	// TODO this used to return a useful error describing which of the
	// commands failed, mostly for the case of an application error like
	// WRONGTYPE.
	var err error
	var errDiscarded resp.ErrDiscarded
	allDiscarded := true
	for _, m := range p.mm {
		if m.err == nil {
			continue
		} else if m.err != nil {
			err = m.err
		}
		if !errors.As(m.err, &errDiscarded) {
			allDiscarded = false
		}
	}

	// unwrap the error if not all of the errors encountered were discarded.
	// Unwrapping is done within a for loop in case the error has multiple
	// levels of ErrDiscarded (somehow).
	//
	// TODO there's probably a more elegant way to do this with errors.Unwrap
	for {
		if err != nil && !allDiscarded && errors.As(err, &errDiscarded) {
			err = errDiscarded.Err
		} else {
			break
		}
	}
	return err
}

////////////////////////////////////////////////////////////////////////////////

type withConn struct {
	key [1]string // use array to avoid allocation in Keys
	fn  func(Conn) error
}

// WithConn is used to perform a set of independent Actions on the same Conn.
//
// key should be a key which one or more of the inner Actions is going to act
// on, or "" if no keys are being acted on or the keys aren't yet known. key is
// generally only necessary when using Cluster.
//
// The callback function is what should actually carry out the inner actions,
// and the error it returns will be passed back up immediately.
//
// NOTE that WithConn only ensures all inner Actions are performed on the same
// Conn, it doesn't make them transactional. Use MULTI/WATCH/EXEC within a
// WithConn for transactions, or use EvalScript
func WithConn(key string, fn func(Conn) error) Action {
	return &withConn{[1]string{key}, fn}
}

func (wc *withConn) Keys() []string {
	return wc.key[:]
}

func (wc *withConn) Perform(c Conn) error {
	return wc.fn(c)
}
