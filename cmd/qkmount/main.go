// Command qkmount is a lightweight, cross-platform cloud/server drive mounter.
//
//	qkmount serve            run the background daemon + web UI
//	qkmount list             list connections and mount status
//	qkmount mount <name>     mount a connection
//	qkmount unmount <name>   unmount a connection
//	qkmount add ...          add a connection from the command line
//	qkmount open             open the web UI
//	qkmount version          print version
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"qkmount/internal/backend"
	"qkmount/internal/config"
	"qkmount/internal/mount"
	"qkmount/internal/oauthflow"
	"qkmount/internal/rclone"
	"qkmount/internal/server"
	"qkmount/internal/tray"
)

const version = "0.1.0"

func main() {
	config.BuildBackend = func(c *config.Connection) (backend.Backend, error) {
		return backend.Build(c.Type, c.Name, c.Options)
	}

	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch args[0] {
	case "__mount":
		// internal: qkmount __mount <configDir> <connID>
		if len(args) < 3 {
			die("__mount requires <configDir> <connID>")
		}
		childMount(args[1], args[2])
	case "serve", "daemon":
		serve(args[1:])
	case "start":
		cmdStart()
	case "stop", "quit":
		cmdStop()
	case "autostart":
		cmdAutostart(args[1:])
	case "list", "ls":
		cmdList()
	case "status":
		cmdStatus()
	case "mount":
		requireName(args, "mount")
		cmdMountOrUnmount(args[1], "mount")
	case "unmount", "umount":
		requireName(args, "unmount")
		cmdMountOrUnmount(args[1], "unmount")
	case "add":
		cmdAdd(args[1:])
	case "auth", "login":
		cmdAuth(args[1:])
	case "import-rclone":
		cmdImportRclone(args[1:])
	case "remove", "rm":
		requireName(args, "remove")
		cmdRemove(args[1])
	case "open", "ui":
		cmdOpen()
	case "version", "--version", "-v":
		fmt.Printf("qkmount %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `qkmount - mount cloud servers and remote drives locally

Usage:
  qkmount serve                 Run the daemon and web UI (foreground)
  qkmount start                 Start the daemon in the background
  qkmount stop                  Stop the background daemon
  qkmount ui                    Open the web UI in a browser
  qkmount list                  List connections and their status
  qkmount mount <name>          Mount a connection
  qkmount unmount <name>        Unmount a connection
  qkmount add [flags]           Add a connection
  qkmount auth <provider> [--name N] [--automount] [--client-id ID --client-secret SEC]
                         Sign in to Google Drive / Dropbox / OneDrive
  qkmount import-rclone         Import remotes from an rclone config
  qkmount autostart on|off      Enable/disable start at login
  qkmount remove <name>         Delete a connection
  qkmount version               Show version

Options for "add":
  --name <name>  --type <sftp|ftp|webdav|s3|...>  --set key=value (repeatable)
  --mountpoint <dir>  --automount  --readonly
`)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "qkmount: "+format+"\n", a...)
	os.Exit(1)
}

func requireName(args []string, cmd string) {
	if len(args) < 2 {
		die("%s requires a connection name", cmd)
	}
}

func configDir() string {
	if v := os.Getenv("QKMOUNT_CONFIG_DIR"); v != "" {
		return v
	}
	return config.DefaultDir()
}

func loadStore() *config.Store {
	s, err := config.Open(configDir())
	if err != nil {
		die("cannot open config: %v", err)
	}
	return s
}

// --------------------------------------------------------------------------
// daemon
// --------------------------------------------------------------------------

func serve(args []string) {
	store := loadStore()
	dir := configDir()
	mgr := mount.NewManager(store, dir)

	st := store.Settings()
	port := st.WebPort
	useTray := true
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &port)
				i++
			}
		case "--no-automount":
			mgr.SetAutoRestart(false)
		case "--tray":
			useTray = true
		case "--no-tray":
			useTray = false
		}
	}

	mgr.StartAllAuto()

	var tr *tray.Tray
	go func() {
		// Periodically ensure auto-mount connections stay mounted.
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			mgr.Reconcile()
			if tr != nil {
				tr.Refresh()
			}
		}
	}()

	srv := &server.Server{Store: store, Mgr: mgr}
	srv.Quit = func() {
		mgr.StopAll()
		os.Exit(0)
	}

	if useTray {
		tr = startTray(mgr, store, st.WebBind, port)
		if tr != nil {
			defer tr.Stop()
		}
	}

	fmt.Printf("QKMount %s listening on http://%s:%d\n", version, st.WebBind, port)
	if err := srv.ListenAndServe(st.WebBind, port); err != nil {
		die("server error: %v", err)
	}
}

// startTray launches the native tray icon, returning nil if unavailable.
func startTray(mgr *mount.Manager, store *config.Store, bind string, port int) *tray.Tray {
	uiURL := fmt.Sprintf("http://%s:%d/", bind, port)
	onAction := func(action string) {
		switch {
		case action == "open":
			cmdOpenURL(uiURL)
		case action == "quit":
			mgr.StopAll()
			time.Sleep(200 * time.Millisecond)
			os.Exit(0)
		case strings.HasPrefix(action, "toggle:"):
			id := strings.TrimPrefix(action, "toggle:")
			c := store.Get(id)
			if c == nil {
				return
			}
			if mgr.IsRunning(id) {
				_ = mgr.Stop(id)
			} else {
				_ = mgr.Start(c)
			}
		}
	}
	items := func() []tray.Item {
		out := []tray.Item{
			{ID: 1, Label: "Open QKMount", Action: "open", Enabled: true},
			{ID: 2, Separator: true},
		}
		running := map[string]bool{}
		for _, inst := range mgr.Snapshot() {
			if inst.Status == mount.StatusMounted || inst.Status == mount.StatusMounting {
				running[inst.ConnID] = true
			}
		}
		conns := store.List()
		id := int32(100)
		for _, c := range conns {
			label := "  " + c.Name
			if running[c.ID] {
				label = "● " + c.Name
			} else {
				label = "○ " + c.Name
			}
			out = append(out, tray.Item{ID: id, Label: label, Action: "toggle:" + c.ID, Enabled: true})
			id++
		}
		out = append(out,
			tray.Item{ID: 5000, Separator: true},
			tray.Item{ID: 5001, Label: "Quit QKMount", Action: "quit", Enabled: true},
		)
		return out
	}
	tr, err := tray.Start(tray.Options{
		Title:      "QKMount",
		IconName:   "folder-remote",
		OnActivate: func() { cmdOpenURL(uiURL) },
		Items:      items,
		OnAction:   onAction,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "qkmount: tray unavailable (%v)\n", err)
		return nil
	}
	return tr
}

func cmdOpenURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println(url)
	}
}

// --------------------------------------------------------------------------
// child: actually mount (blocks)
// --------------------------------------------------------------------------

func childMount(dir, connID string) {
	store, err := config.Open(dir)
	if err != nil {
		die("child: %v", err)
	}
	c := store.Get(connID)
	if c == nil {
		die("child: connection %q not found", connID)
	}
	be, err := config.BuildBackend(c)
	if err != nil {
		die("child: %v", err)
	}
	fmt.Printf("connecting %s (%s)...\n", c.Name, c.Type)
	if err := be.Init(); err != nil {
		die("child: connection failed: %v", err)
	}
	if c.MountPoint == "" {
		die("child: no mount point")
	}
	if err := os.MkdirAll(c.MountPoint, 0o755); err != nil {
		die("child: %v", err)
	}
	fmt.Printf("mounted %s at %s\n", c.Name, c.MountPoint)
	if err := mount.Serve(be, c.Name, c.MountPoint, c.ReadOnly); err != nil {
		die("child: %v", err)
	}
}

// --------------------------------------------------------------------------
// simple CLI commands
// --------------------------------------------------------------------------

func cmdList() {
	store := loadStore()
	if snap, ok := fetchDaemonStatus(); ok {
		for _, inst := range snap {
			fmt.Printf("%-20s %-8s %-10s %s\n", inst.Name, inst.Type, inst.Status, inst.MountPoint)
		}
		return
	}
	mgr := mount.NewManager(store, configDir())
	for _, inst := range mgr.Snapshot() {
		fmt.Printf("%-20s %-8s %-10s %s\n", inst.Name, inst.Type, inst.Status, inst.MountPoint)
	}
}

// fetchDaemonStatus returns the daemon's live status snapshot if reachable.
func fetchDaemonStatus() ([]mount.Instance, bool) {
	store := loadStore()
	st := store.Settings()
	url := fmt.Sprintf("http://%s:%d/api/status", st.WebBind, st.WebPort)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var out []mount.Instance
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false
	}
	return out, true
}

func cmdStatus() {
	if snap, ok := fetchDaemonStatus(); ok {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
		return
	}
	store := loadStore()
	mgr := mount.NewManager(store, configDir())
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(mgr.Snapshot())
}

func uiURL() string {
	store := loadStore()
	st := store.Settings()
	return fmt.Sprintf("http://%s:%d/", st.WebBind, st.WebPort)
}

func cmdOpen() {
	url := uiURL()
	if _, ok := fetchDaemonStatus(); !ok {
		cmdStart()
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println(url)
	}
}

// daemonRequest tries to reach a running daemon; returns false if unavailable.
func daemonRequest(method, path string, body any) bool {
	store := loadStore()
	st := store.Settings()
	url := fmt.Sprintf("http://%s:%d%s", st.WebBind, st.WebPort, path)
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req, err := http.NewRequest(method, url, buf)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}

func cmdMountOrUnmount(name, action string) {
	store := loadStore()
	c := store.Get(name)
	if c == nil {
		die("no such connection: %s", name)
	}
	if daemonRequest("POST", "/api/connections/"+c.ID+"/"+action, nil) {
		fmt.Printf("%s: %s requested\n", c.Name, action)
		return
	}
	// No daemon: do it directly.
	if action == "unmount" {
		mount.ForceUnmount(c.MountPoint)
		fmt.Printf("unmounted %s\n", c.Name)
		return
	}
	// Foreground mount.
	be, err := config.BuildBackend(c)
	if err != nil {
		die("%v", err)
	}
	if err := be.Init(); err != nil {
		die("connection failed: %v", err)
	}
	_ = os.MkdirAll(c.MountPoint, 0o755)
	if err := mount.Serve(be, c.Name, c.MountPoint, c.ReadOnly); err != nil {
		die("%v", err)
	}
}

func cmdAdd(args []string) {
	store := loadStore()
	c := &config.Connection{Options: map[string]string{}}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++
			c.Name = args[i]
		case "--type":
			i++
			c.Type = args[i]
		case "--set":
			i++
			k, v, ok := stringsCut(args[i], "=")
			if ok {
				c.Options[k] = v
			}
		case "--mountpoint":
			i++
			c.MountPoint = args[i]
		case "--automount":
			c.AutoMount = true
		case "--readonly":
			c.ReadOnly = true
		}
	}
	if c.Name == "" || c.Type == "" {
		die("add: --name and --type are required")
	}
	if err := store.Add(c); err != nil {
		die("add: %v", err)
	}
	fmt.Printf("added %q (%s) -> %s\n", c.Name, c.Type, c.MountPoint)
}

// cmdStart launches the daemon in the background if it is not already running.
func cmdStart() {
	if _, ok := fetchDaemonStatus(); ok {
		fmt.Println("QKMount is already running")
		return
	}
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	dir := configDir()
	_ = os.MkdirAll(dir, 0o700)
	logPath := filepath.Join(dir, "daemon.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		die("cannot open log: %v", err)
	}
	cmd := exec.Command(self, "serve")
	cmd.Env = append(os.Environ(), "QKMOUNT_CONFIG_DIR="+dir)
	cmd.Stdout = logf
	cmd.Stderr = logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		die("cannot start daemon: %v", err)
	}
	// Wait briefly for the API to come up.
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		if _, ok := fetchDaemonStatus(); ok {
			fmt.Printf("QKMount started (log: %s)\n", logPath)
			return
		}
	}
	fmt.Printf("QKMount starting (log: %s)\n", logPath)
}

// cmdStop stops a running daemon.
func cmdStop() {
	if _, ok := fetchDaemonStatus(); !ok {
		fmt.Println("QKMount is not running")
		return
	}
	if daemonRequest("POST", "/api/quit", nil) {
		fmt.Println("QKMount stopped")
		return
	}
	die("could not stop QKMount")
}

// cmdAutostart manages the user's login autostart entry.
func cmdAutostart(args []string) {
	if len(args) == 0 {
		die("autostart requires 'on' or 'off'")
	}
	base, err := os.UserConfigDir()
	if err != nil {
		die("autostart: %v", err)
	}
	dir := filepath.Join(base, "autostart")
	path := filepath.Join(dir, "qkmount.desktop")
	switch args[0] {
	case "on", "enable":
		if err := os.MkdirAll(dir, 0o755); err != nil {
			die("autostart: %v", err)
		}
		self, err := os.Executable()
		if err != nil {
			self = "qkmount"
		}
		entry := "[Desktop Entry]\nType=Application\nName=QKMount\nComment=Mount cloud drives locally\n" +
			"Exec=" + self + " serve\nIcon=qkmount\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"
		if err := os.WriteFile(path, []byte(entry), 0o644); err != nil {
			die("autostart: %v", err)
		}
		fmt.Printf("autostart enabled (%s)\n", path)
	case "off", "disable":
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			die("autostart: %v", err)
		}
		fmt.Println("autostart disabled")
	default:
		die("autostart: unknown action %q", args[0])
	}
}

// cmdRemove deletes a saved connection.
func cmdRemove(name string) {
	store := loadStore()
	c := store.Get(name)
	if c == nil {
		die("no such connection: %s", name)
	}
	if daemonRequest("POST", "/api/connections/"+c.ID+"/unmount", nil) {
		// best effort
	}
	if err := store.Remove(c.ID); err != nil {
		die("remove: %v", err)
	}
	fmt.Printf("removed %q\n", name)
}

// cmdAuth runs an interactive OAuth sign-in and saves the resulting connection.
func cmdAuth(args []string) {
	if len(args) == 0 {
		die("auth requires a provider: gdrive, dropbox or onedrive")
	}
	kind := args[0]
	name := ""
	mountpoint := ""
	automount := false
	clientID := ""
	clientSecret := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "--mountpoint":
			if i+1 < len(args) {
				mountpoint = args[i+1]
				i++
			}
		case "--client-id":
			if i+1 < len(args) {
				clientID = args[i+1]
				i++
			}
		case "--client-secret":
			if i+1 < len(args) {
				clientSecret = args[i+1]
				i++
			}
		case "--automount":
			automount = true
		}
	}

	prov, err := oauthflow.ProviderParams(kind)
	if err != nil {
		die("%v", err)
	}
	if clientID != "" {
		prov.Params.ClientID = clientID
		prov.Params.ClientSecret = clientSecret
	}

	fmt.Printf("Signing in to %s...\n", prov.Label)
	fmt.Println("Your browser will open. Approve access, then come back here.")
	if prov.Kind == "gdrive" && prov.Params.ClientID == oauthflow.DefaultGoogleClientID {
		fmt.Println("Using the built-in shared Google client (heavily rate-limited).")
		fmt.Println("For reliable access add:  --client-id <ID> --client-secret <SECRET>")
	}
	fmt.Println()

	res, err := oauthflow.Run(context.Background(), prov.Params, func(u string) error {
		fmt.Printf("If your browser does not open, visit this URL:\n\n  %s\n\n", u)
		return oauthflow.OpenBrowser(u)
	})
	if err != nil {
		die("sign-in failed: %v", err)
	}
	tokenJSON, err := oauthflow.TokenJSON(res.Token)
	if err != nil {
		die("sign-in: %v", err)
	}

	opts := map[string]string{"token": tokenJSON}
	// Only persist client credentials when they are custom (non-default), so
	// the backend keeps using the shared defaults for refresh otherwise.
	if prov.Kind == "gdrive" {
		if email, err := backend.GoogleAccountEmail(tokenJSON, prov.Params.ClientID, prov.Params.ClientSecret); err == nil && email != "" {
			if name == "" {
				name = email
			}
			fmt.Printf("Signed in as %s\n", email)
		}
		if prov.Params.ClientID != oauthflow.DefaultGoogleClientID {
			opts["client_id"] = prov.Params.ClientID
			opts["client_secret"] = prov.Params.ClientSecret
		}
	}
	if name == "" {
		name = prov.Label
	}

	store := loadStore()
	if existing := store.Get(name); existing != nil {
		die("a connection named %q already exists (use --name to choose another)", name)
	}
	c := &config.Connection{
		Name:       name,
		Type:       prov.Kind,
		Options:    opts,
		MountPoint: mountpoint,
		AutoMount:  automount,
	}
	if err := store.Add(c); err != nil {
		die("save: %v", err)
	}
	fmt.Printf("Added connection %q (%s) -> %s\n", c.Name, c.Type, c.MountPoint)
	fmt.Printf("Mount it with:  qkmount mount %q   (or use the web UI)\n", c.Name)
}

// cmdImportRclone imports remotes from an rclone config file.
func cmdImportRclone(args []string) {
	path := rclone.DefaultPath()
	dryRun := false
	overwrite := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config":
			if i+1 < len(args) {
				path = args[i+1]
				i++
			}
		case "--dry-run":
			dryRun = true
		case "--overwrite":
			overwrite = true
		}
	}
	remotes, err := rclone.Parse(path)
	if err != nil {
		die("cannot read rclone config %s: %v", path, err)
	}
	store := loadStore()
	added, skipped, unsupported := 0, 0, 0
	for _, r := range remotes {
		c, ok := rclone.Convert(r)
		if !ok {
			fmt.Printf("skip    %-20s (unsupported type %q)\n", r.Name, r.Items["type"])
			unsupported++
			continue
		}
		if existing := store.Get(c.Name); existing != nil {
			if !overwrite {
				fmt.Printf("exists  %-20s (%s) - use --overwrite to replace\n", c.Name, c.Type)
				skipped++
				continue
			}
			c.ID = existing.ID
			if c.MountPoint == "" {
				c.MountPoint = existing.MountPoint
			}
			if err := store.Update(c); err != nil {
				fmt.Printf("error   %-20s %v\n", r.Name, err)
				continue
			}
			fmt.Printf("update  %-20s (%s)\n", c.Name, c.Type)
			added++
			continue
		}
		if dryRun {
			fmt.Printf("would   %-20s (%s)\n", c.Name, c.Type)
			added++
			continue
		}
		if err := store.Add(c); err != nil {
			fmt.Printf("error   %-20s %v\n", r.Name, err)
			continue
		}
		fmt.Printf("added   %-20s (%s) -> %s\n", c.Name, c.Type, c.MountPoint)
		added++
	}
	verb := "imported"
	if dryRun {
		verb = "would import"
	}
	fmt.Printf("\n%s %d remote(s); skipped %d, unsupported %d\n", verb, added, skipped, unsupported)
}

func stringsCut(s, sep string) (string, string, bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

var _ = filepath.Join
