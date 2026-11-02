// Command fakeportal answers FileChooser.OpenFile on a private bus for e2e.py: it
// picks os.Args[1], and refuses a filter that isn't the portal's a(sa(us)).
package main

import (
	"fmt"
	"os"

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
	err := dbus.Store([]any{options["filters"].Value()}, &filters)
	if err != nil || len(filters) == 0 || len(filters[0].Rules) == 0 {
		return "", dbus.MakeFailedError(fmt.Errorf("filters is not a(sa(us)): %v", err))
	}
	token, _ := options["handle_token"].Value().(string)
	name := []byte(string(sender)[1:])
	for i, b := range name {
		if b == '.' {
			name[i] = '_'
		}
	}
	request := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + string(name) + "/" + token)
	go c.conn.Emit(request, "org.freedesktop.portal.Request.Response", uint32(0),
		map[string]dbus.Variant{"uris": dbus.MakeVariant([]string{"file://" + c.file})})
	fmt.Printf("picked for %q\n", title)
	return request, nil
}

func main() {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	err = conn.Export(chooser{conn, os.Args[1]}, "/org/freedesktop/portal/desktop", "org.freedesktop.portal.FileChooser")
	if err == nil {
		_, err = conn.RequestName("org.freedesktop.portal.Desktop", dbus.NameFlagDoNotQueue)
	}
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("ready")
	select {}
}
