// Command fakeportal answers FileChooser on a private bus for e2e.py. OpenFile picks
// os.Args[1], its folder when asked for one, or os.Args[1] + ".unfiltered" when asked with no
// filter, so a test can tell which it got. It refuses a filter that isn't the portal's
// a(sa(us)), and a folder request with a filter or a directory option that isn't a bool.
// SaveFile saves as the current_name it was given, in os.Args[1]'s folder. Settings answers
// Read from what stdin's "set <namespace> <key> <JSON value>" lines last set, and says so
// with SettingChanged.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

type chooser struct {
	conn *dbus.Conn
	file string
}

func (c chooser) OpenFile(sender dbus.Sender, parent, title string, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	var filters []struct {
		Name  string
		Rules []struct {
			Kind    uint32
			Pattern string
		}
	}
	picked := c.file
	if option, ok := options["directory"]; ok {
		if folder, isBool := option.Value().(bool); !isBool || !folder {
			return "", dbus.MakeFailedError(fmt.Errorf("directory is not true: %v", option))
		}
		if _, ok := options["filters"]; ok {
			return "", dbus.MakeFailedError(fmt.Errorf("a filter on a folder request"))
		}
		picked = filepath.Dir(c.file)
	} else if _, ok := options["filters"]; !ok {
		picked = c.file + ".unfiltered"
	} else {
		err := dbus.Store([]any{options["filters"].Value()}, &filters)
		if err != nil || len(filters) == 0 || len(filters[0].Rules) == 0 {
			return "", dbus.MakeFailedError(fmt.Errorf("filters is not a(sa(us)): %v", err))
		}
	}
	return c.answer(sender, options, picked, title)
}

func (c chooser) SaveFile(sender dbus.Sender, parent, title string, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	name, ok := options["current_name"].Value().(string)
	if !ok || name == "" || filepath.Base(name) != name {
		return "", dbus.MakeFailedError(fmt.Errorf("current_name is not a file name: %v", options["current_name"]))
	}
	return c.answer(sender, options, filepath.Join(filepath.Dir(c.file), name), title)
}

// answer is the Response the portal sends on the request path the caller predicted.
func (c chooser) answer(sender dbus.Sender, options map[string]dbus.Variant, picked, title string) (dbus.ObjectPath, *dbus.Error) {
	token, _ := options["handle_token"].Value().(string)
	name := []byte(string(sender)[1:])
	for i, b := range name {
		if b == '.' {
			name[i] = '_'
		}
	}
	request := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + string(name) + "/" + token)
	go c.conn.Emit(request, "org.freedesktop.portal.Request.Response", uint32(0),
		map[string]dbus.Variant{"uris": dbus.MakeVariant([]string{"file://" + picked})})
	fmt.Printf("picked for %q\n", title)
	return request, nil
}

// settings is the portal's Settings: values by namespace and key.
type settings struct {
	mu     sync.Mutex
	values map[[2]string]any
}

// Read answers a value wrapped in a second variant, as the real portal's Read does.
func (s *settings) Read(namespace, key string) (dbus.Variant, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[[2]string{namespace, key}]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.portal.Error.NotFound", []any{"no such setting"})
	}
	return dbus.MakeVariant(dbus.MakeVariant(v)), nil
}

// set reads stdin's lines and changes the settings they name, telling the bus.
func (s *settings) set(conn *dbus.Conn) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 4)
		var v any
		if len(f) != 4 || f[0] != "set" || json.Unmarshal([]byte(f[3]), &v) != nil {
			fmt.Println("bad line:", sc.Text())
			continue
		}
		s.mu.Lock()
		s.values[[2]string{f[1], f[2]}] = v
		s.mu.Unlock()
		conn.Emit("/org/freedesktop/portal/desktop", "org.freedesktop.portal.Settings.SettingChanged", f[1], f[2], dbus.MakeVariant(v))
		fmt.Println("set", f[1], f[2])
	}
}

func main() {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	err = conn.Export(chooser{conn, os.Args[1]}, "/org/freedesktop/portal/desktop", "org.freedesktop.portal.FileChooser")
	st := &settings{values: map[[2]string]any{}}
	if err == nil {
		err = conn.Export(st, "/org/freedesktop/portal/desktop", "org.freedesktop.portal.Settings")
	}
	if err == nil {
		_, err = conn.RequestName("org.freedesktop.portal.Desktop", dbus.NameFlagDoNotQueue)
	}
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("ready")
	go st.set(conn)
	<-conn.Context().Done()
}
