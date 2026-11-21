package mpv

import (
	"bytes"
	"strings"
	"sync"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// script is the shipped watchparty.lua, run by gopher-lua under a fake mpv (driver_test.go's
// node, party_test.go's simNode), so the fakes model only mpv and every rule of the script
// lives once. It hears mpv as a script in mpv does: one event at a time, each handler run to
// its end, and what comes meanwhile (a set's report, a seek's reply) waits in its queue.
type script struct {
	l   *lua.LState
	mpv scriptMpv
	// queue: what the script still has to hear; applying: it is hearing one now; idle: what it
	// hears once the queue is empty, as mpv sends a property's change then; marked: a pause
	// change mark it hasn't heard yet, which another one folds into
	queue  []func()
	busy   bool
	idle   []func()
	marked bool
	// what it registered: script messages, events, the pause observer, and the async seeks
	// waiting for their reply, by the number mpv's seek was given
	messages map[string]*lua.LFunction
	events   map[string][]*lua.LFunction
	pause    []*lua.LFunction
	replies  map[int]*lua.LFunction
	seeks    int
}

// scriptMpv is mpv as the script reaches it through mp. The fake answers each call at once, as
// mpv does, and tells the script what comes of it later, through script's methods.
type scriptMpv interface {
	// get is a property as mp.get_property reads it, false if mpv has none
	get(name string) (string, bool)
	// set is mp.set_property; the script never reads its result
	set(name, value string)
	// seek is mp.command_native_async's seek to second; its reply is replied(n, ok)
	seek(second string, n int)
	// say is the script's script-message, to every client
	say(args []string)
}

// compiled is watchparty.lua compiled once: a FunctionProto is never changed, so every
// LState can run the same one.
var compiled = sync.OnceValues(func() (*lua.FunctionProto, error) {
	chunk, err := parse.Parse(bytes.NewReader(Script), "watchparty.lua")
	if err != nil {
		return nil, err
	}
	return lua.Compile(chunk, "watchparty.lua")
})

// newScript loads the script for m, as mpv does at its start: its main chunk runs, and its
// pause observer fires once.
func newScript(m scriptMpv) *script {
	s := &script{l: lua.NewState(lua.Options{SkipOpenLibs: true, CallStackSize: 16, RegistrySize: 128, RegistryMaxSize: 1 << 14}), mpv: m, messages: map[string]*lua.LFunction{}, events: map[string][]*lua.LFunction{}, replies: map[int]*lua.LFunction{}}
	for name, open := range map[string]lua.LGFunction{lua.BaseLibName: lua.OpenBase, lua.TabLibName: lua.OpenTable, lua.StringLibName: lua.OpenString} {
		s.l.Push(s.l.NewFunction(open))
		s.l.Push(lua.LString(name))
		s.l.Call(1, 0)
	}
	mp := s.l.NewTable()
	s.l.SetFuncs(mp, map[string]lua.LGFunction{
		"get_property": func(l *lua.LState) int {
			if v, ok := s.mpv.get(l.CheckString(1)); ok {
				l.Push(lua.LString(v))
				return 1
			}
			if l.GetTop() >= 2 {
				l.Push(l.Get(2))
				return 1
			}
			l.Push(lua.LNil)
			l.Push(lua.LString("property unavailable"))
			return 2
		},
		"set_property": func(l *lua.LState) int {
			s.mpv.set(l.CheckString(1), l.ToString(2))
			l.Push(lua.LTrue)
			return 1
		},
		"commandv": func(l *lua.LState) int {
			args := make([]string, l.GetTop())
			for i := range args {
				args[i] = l.ToString(i + 1)
			}
			if len(args) > 0 && args[0] == "script-message" {
				s.mpv.say(args[1:])
			}
			return 0
		},
		"command_native_async": func(l *lua.LState) int {
			cmd := l.CheckTable(1)
			if cmd.RawGetInt(1).String() != "seek" {
				l.RaiseError("the fake runs only a seek async, not %s", cmd.RawGetInt(1))
			}
			s.seeks++
			s.replies[s.seeks] = l.CheckFunction(2)
			s.mpv.seek(cmd.RawGetInt(2).String(), s.seeks)
			l.Push(lua.LNumber(s.seeks))
			return 1
		},
		"observe_property": func(l *lua.LState) int {
			if name := l.CheckString(1); name != pause {
				l.RaiseError("the fake observes only pause, not %s", name)
			}
			fn := l.CheckFunction(3)
			s.pause = append(s.pause, fn)
			// mpv says what a property holds as it is observed
			s.post(func() { s.call(fn, lua.LString(pause)) })
			return 0
		},
		"register_script_message": func(l *lua.LState) int {
			s.messages[l.CheckString(1)] = l.CheckFunction(2)
			return 0
		},
		"register_event": func(l *lua.LState) int {
			name := l.CheckString(1)
			s.events[name] = append(s.events[name], l.CheckFunction(2))
			return 0
		},
		"get_script_name": func(l *lua.LState) int {
			l.Push(lua.LString(scriptName))
			return 1
		},
	})
	s.l.SetGlobal("mp", mp)
	proto, err := compiled()
	if err != nil {
		panic(err)
	}
	s.busy = true
	s.l.Push(s.l.NewFunctionFromProto(proto))
	if err := s.l.PCall(0, 0, nil); err != nil {
		panic(err)
	}
	s.busy = false
	s.run()
	return s
}

// message is a script-message-to the script, as the node sends it.
func (s *script) message(name string, args ...string) {
	s.post(func() {
		if fn := s.messages[name]; fn != nil {
			vals := make([]lua.LValue, len(args))
			for i, a := range args {
				vals[i] = lua.LString(a)
			}
			s.call(fn, vals...)
		}
	})
}

// event is one of mpv's events, with its fields (start-file's playlist_entry_id, end-file's
// reason).
func (s *script) event(name string, fields map[string]lua.LValue) {
	s.post(func() {
		for _, fn := range s.events[name] {
			e := s.l.NewTable()
			e.RawSetString("event", lua.LString(name))
			for k, v := range fields {
				e.RawSetString(k, v)
			}
			s.call(fn, e)
		}
	})
}

// pauseMarked is mpv marking pause changed: the observer fires, also when the value ends
// where it was (format "none"); marked again before it fires, it fires once.
func (s *script) pauseMarked() {
	if s.marked {
		return
	}
	s.marked = true
	s.post(func() {
		s.marked = false
		for _, fn := range s.pause {
			s.call(fn, lua.LString(pause))
		}
	})
}

// replied is mpv's reply to the script's seek n.
func (s *script) replied(n int, ok bool) {
	s.post(func() {
		fn := s.replies[n]
		delete(s.replies, n)
		s.call(fn, lua.LBool(ok))
	})
}

// post queues what the script hears next, and runs it unless the script is hearing one now:
// then it comes once that handler has returned.
func (s *script) post(f func()) {
	s.queue = append(s.queue, f)
	s.run()
}

// whenIdle is f once the script has heard everything queued.
func (s *script) whenIdle(f func()) {
	s.idle = append(s.idle, f)
	s.run()
}

func (s *script) run() {
	if s.busy {
		return
	}
	s.busy = true
	for len(s.queue) > 0 || len(s.idle) > 0 {
		if len(s.queue) == 0 {
			s.queue, s.idle = s.idle, nil
		}
		f := s.queue[0]
		s.queue = s.queue[1:]
		f()
	}
	s.busy = false
}

func (s *script) call(fn *lua.LFunction, args ...lua.LValue) {
	if err := s.l.CallByParam(lua.P{Fn: fn, Protect: true}, args...); err != nil {
		panic("watchparty.lua: " + strings.TrimSpace(err.Error()))
	}
}
