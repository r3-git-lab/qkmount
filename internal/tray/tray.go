// Package tray implements a native system-tray icon for QKMount using the
// freedesktop StatusNotifierItem + com.canonical.dbusmenu protocols over the
// session D-Bus.  It needs no C libraries or GTK headers, so it stays
// lightweight and cross-platform (on non-Linux systems it simply reports that
// no tray host is available and the daemon keeps running).
package tray

import (
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	sniPath   = dbus.ObjectPath("/StatusNotifierItem")
	menuPath  = dbus.ObjectPath("/MenuBar")
	sniIface  = "org.kde.StatusNotifierItem"
	menuIface = "com.canonical.dbusmenu"
	propIface = "org.freedesktop.DBus.Properties"

	watcherName = "org.x.StatusNotifierWatcher"
	watcherKDE  = "org.kde.StatusNotifierWatcher"
	watcherPath = dbus.ObjectPath("/StatusNotifierWatcher")
)

// Item is one clickable entry in the tray menu.
type Item struct {
	ID        int32
	Label     string
	Action    string
	Enabled   bool
	Separator bool
}

// Tray is a running tray icon.
type Tray struct {
	conn    *dbus.Conn
	busName string

	onActivate func()
	items      func() []Item
	onAction   func(string)

	mu       sync.Mutex
	revision uint32
}

// Options configures a tray.
type Options struct {
	Title      string
	IconName   string
	OnActivate func()
	Items      func() []Item
	OnAction   func(action string)
}

// Start connects to the session bus and registers a StatusNotifierItem.
func Start(opts Options) (*Tray, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("tray: no session bus: %w", err)
	}
	t := &Tray{
		conn:       conn,
		onActivate: opts.OnActivate,
		items:      opts.Items,
		onAction:   opts.OnAction,
		revision:   1,
	}
	if t.items == nil {
		t.items = func() []Item { return nil }
	}
	if opts.IconName == "" {
		opts.IconName = "folder-remote"
	}

	name := fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid())
	reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		return nil, fmt.Errorf("tray: could not own bus name %q", name)
	}
	t.busName = name

	if err := conn.Export(&sniObject{t: t}, sniPath, sniIface); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Export(&propObject{t: t, icon: opts.IconName, title: opts.Title}, sniPath, propIface); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Export(&menuObject{t: t}, menuPath, menuIface); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Export(&propObject{t: t, icon: opts.IconName, title: opts.Title}, menuPath, propIface); err != nil {
		conn.Close()
		return nil, err
	}

	// Register with whichever watcher this desktop provides.
	if err := t.register(); err != nil {
		conn.Close()
		return nil, err
	}
	return t, nil
}

func (t *Tray) register() error {
	watchers := []struct {
		dest  string
		iface string
	}{
		{watcherName, watcherKDE},
		{"org.kde.StatusNotifierWatcher", watcherKDE},
	}
	var lastErr error
	for _, w := range watchers {
		obj := t.conn.Object(w.dest, watcherPath)
		if err := obj.Call(w.iface+".RegisterStatusNotifierItem", 0, t.busName).Err; err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("tray: no StatusNotifierWatcher: %w", lastErr)
}

// Stop unregisters and closes the tray.
func (t *Tray) Stop() {
	if t.conn != nil {
		t.conn.ReleaseName(t.busName)
		t.conn.Close()
	}
}

// Refresh bumps the menu revision and notifies the host to redraw.
func (t *Tray) Refresh() {
	t.mu.Lock()
	t.revision++
	rev := t.revision
	t.mu.Unlock()
	t.conn.Emit(menuPath, menuIface+".LayoutUpdated", rev, int32(0))
	t.conn.Emit(sniPath, sniIface+".NewStatus", "Active")
}

// ---------------------------------------------------------------------------
// org.kde.StatusNotifierItem
// ---------------------------------------------------------------------------

type sniObject struct{ t *Tray }

func (o *sniObject) Activate(x, y int32) *dbus.Error {
	if o.t.onActivate != nil {
		go o.t.onActivate()
	}
	return nil
}
func (o *sniObject) SecondaryActivate(x, y int32) *dbus.Error { return nil }
func (o *sniObject) ContextMenu(x, y int32) *dbus.Error       { return nil }
func (o *sniObject) Scroll(delta int32, orientation string) *dbus.Error {
	return nil
}

// ---------------------------------------------------------------------------
// org.freedesktop.DBus.Properties (shared by the SNI and menu objects)
// ---------------------------------------------------------------------------

type propObject struct {
	t     *Tray
	icon  string
	title string
}

type iconPixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

type toolTip struct {
	IconName string
	IconPix  []iconPixmap
	Title    string
	Subtitle string
}

func (o *propObject) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	all, err := o.GetAll(iface)
	if err != nil {
		return dbus.MakeVariant(""), err
	}
	v, ok := all[name]
	if !ok {
		return dbus.MakeVariant(""), dbus.MakeFailedError(fmt.Errorf("no such property %q", name))
	}
	return v, nil
}

func (o *propObject) Set(iface, name string, v dbus.Variant) *dbus.Error {
	return nil
}

func (o *propObject) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	switch iface {
	case sniIface:
		return map[string]dbus.Variant{
			"Category":          dbus.MakeVariant("ApplicationStatus"),
			"Id":                dbus.MakeVariant("qkmount"),
			"Title":             dbus.MakeVariant(nonEmpty(o.title, "QKMount")),
			"Status":            dbus.MakeVariant("Active"),
			"IconName":          dbus.MakeVariant(o.icon),
			"IconPixmap":        dbus.MakeVariant([]iconPixmap{}),
			"OverlayIconName":   dbus.MakeVariant(""),
			"AttentionIconName": dbus.MakeVariant(""),
			"ToolTip":           dbus.MakeVariant(toolTip{Title: nonEmpty(o.title, "QKMount"), Subtitle: "Cloud drives & servers"}),
			"ItemIsMenu":        dbus.MakeVariant(false),
			"Menu":              dbus.MakeVariant(menuPath),
			"WindowId":          dbus.MakeVariant(int32(0)),
		}, nil
	case menuIface:
		return map[string]dbus.Variant{
			"Version":       dbus.MakeVariant(uint32(3)),
			"TextDirection": dbus.MakeVariant("ltr"),
			"Status":        dbus.MakeVariant("normal"),
			"IconThemePath": dbus.MakeVariant([]string{}),
		}, nil
	}
	return map[string]dbus.Variant{}, nil
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------------------
// com.canonical.dbusmenu
// ---------------------------------------------------------------------------

type menuObject struct{ t *Tray }

type menuLayout struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

type groupProp struct {
	ID    int32
	Props map[string]dbus.Variant
}

func (o *menuObject) GetLayout(parentID int32, depth int32, props []string) (uint32, menuLayout, *dbus.Error) {
	items := o.t.items()
	children := make([]dbus.Variant, 0, len(items))
	for _, it := range items {
		children = append(children, dbus.MakeVariant(menuLayout{
			ID:    it.ID,
			Props: itemProps(it),
		}))
	}
	root := menuLayout{
		ID:       parentID,
		Props:    map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")},
		Children: children,
	}
	o.t.mu.Lock()
	rev := o.t.revision
	o.t.mu.Unlock()
	return rev, root, nil
}

func (o *menuObject) GetGroupProperties(ids []int32, props []string) ([]groupProp, *dbus.Error) {
	want := map[int32]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []groupProp
	for _, it := range o.t.items() {
		if len(want) > 0 && !want[it.ID] {
			continue
		}
		out = append(out, groupProp{ID: it.ID, Props: itemProps(it)})
	}
	return out, nil
}

func (o *menuObject) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	for _, it := range o.t.items() {
		if it.ID == id && it.Action != "" && o.t.onAction != nil {
			go o.t.onAction(it.Action)
			return nil
		}
	}
	return nil
}

func (o *menuObject) AboutToShow(id int32) (bool, *dbus.Error) { return false, nil }

func (o *menuObject) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}

func itemProps(it Item) map[string]dbus.Variant {
	if it.Separator {
		return map[string]dbus.Variant{"type": dbus.MakeVariant("separator")}
	}
	return map[string]dbus.Variant{
		"label":   dbus.MakeVariant(it.Label),
		"enabled": dbus.MakeVariant(it.Enabled),
		"visible": dbus.MakeVariant(true),
		"type":    dbus.MakeVariant("standard"),
	}
}

var _ = sort.Ints
