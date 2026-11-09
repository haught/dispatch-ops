package runner

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"dispatch-ops/internal/runner/runnerapi"
)

// command is what the runner runs for a job.
type command struct {
	argv []string
	dir  string
	env  []string
	// eventMarker prefixes the lines in the command's output that are
	// structured events (see consume); empty if it writes none.
	eventMarker string
}

// buildCommand returns the command for the job's type.
func buildCommand(spec *runnerapi.RunnerJobSpec, ws *workspace) (command, error) {
	switch spec.JobType {
	case runnerapi.Container:
		if len(spec.Command) == 0 {
			return command{}, errors.New("The job has no command")
		}
		return command{argv: spec.Command, dir: ws.dir, env: ws.env}, nil
	case runnerapi.Script:
		return scriptCommand(spec, ws)
	case runnerapi.Ansible:
		return ansibleCommand(spec, ws)
	case runnerapi.InventorySync:
		return inventorySyncCommand(spec, ws)
	}
	return command{}, fmt.Errorf("This runner does not support %q jobs", spec.JobType)
}

// projectFile resolves the job's entrypoint in the project checkout and
// checks that it is a file there.
func projectFile(spec *runnerapi.RunnerJobSpec, ws *workspace, what string) (string, error) {
	return projectPath(spec, ws, what, false)
}

// projectPath resolves the job's entrypoint in the project checkout and
// checks that it is a file there, or a directory if dirOK.
func projectPath(spec *runnerapi.RunnerJobSpec, ws *workspace, what string, dirOK bool) (string, error) {
	if ws.projectDir == "" || spec.Entrypoint == "" {
		return "", fmt.Errorf("The job has no project or no %s", what)
	}
	if !filepath.IsLocal(spec.Entrypoint) {
		return "", fmt.Errorf("The %s path %q is not inside the project", what, spec.Entrypoint)
	}
	path := filepath.Join(ws.projectDir, spec.Entrypoint)
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("The %s %s is not in the project", what, spec.Entrypoint)
	case err != nil:
		return "", err
	case dirOK && fi.IsDir():
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("The %s %s is not a file", what, spec.Entrypoint)
	}
	return path, nil
}

// --- Script jobs -------------------------------------------------------------

// scriptCommand runs the script with its arguments, in the project
// directory. Scripts with a #! line run with that interpreter; others with
// /bin/sh.
func scriptCommand(spec *runnerapi.RunnerJobSpec, ws *workspace) (command, error) {
	path, err := projectFile(spec, ws, "script")
	if err != nil {
		return command{}, err
	}
	argv := append([]string{path}, spec.Arguments...)
	shebang, err := hasShebang(path)
	if err != nil {
		return command{}, err
	}
	if shebang {
		// Git may not have kept the executable bit (e.g. a repo made on Windows).
		if err := os.Chmod(path, 0o700); err != nil {
			return command{}, err
		}
	} else {
		argv = append([]string{"/bin/sh"}, argv...)
	}
	return command{argv: argv, dir: ws.projectDir, env: ws.env}, nil
}

func hasShebang(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	b := make([]byte, 2)
	n, _ := f.Read(b)
	return n == 2 && string(b) == "#!", nil
}

// --- Ansible jobs ------------------------------------------------------------

//go:embed callback/dispatch_events.py
var callbackPlugin []byte

// ansibleCommand runs ansible-playbook on the playbook, in the project
// directory (so the project's ansible.cfg applies), with the job's
// inventory, extra vars, options and credentials. The callback plugin
// reports structured events in the output.
func ansibleCommand(spec *runnerapi.RunnerJobSpec, ws *workspace) (command, error) {
	playbook, err := projectFile(spec, ws, "playbook")
	if err != nil {
		return command{}, err
	}
	argv := []string{"ansible-playbook"}
	if spec.Inventory != nil {
		inv := ws.path("inventory.json")
		if err := writeJSON(inv, staticInventory(*spec.Inventory), 0o600); err != nil {
			return command{}, err
		}
		argv = append(argv, "-i", inv)
	}
	argv = append(argv, "-e", "@"+ws.vars)
	if spec.Limit != "" {
		argv = append(argv, "--limit", spec.Limit)
	}
	if spec.Tags != "" {
		argv = append(argv, "--tags", spec.Tags)
	}
	if spec.SkipTags != "" {
		argv = append(argv, "--skip-tags", spec.SkipTags)
	}
	if spec.Verbosity > 0 {
		argv = append(argv, "-"+strings.Repeat("v", min(spec.Verbosity, 5)))
	}
	credArgs, err := ansibleCredentialArgs(spec.Credentials, ws)
	if err != nil {
		return command{}, err
	}
	argv = append(argv, credArgs...)
	argv = append(argv, playbook)

	pluginDir := ws.path("callback_plugins")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		return command{}, err
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "dispatch_events.py"), callbackPlugin, 0o600); err != nil {
		return command{}, err
	}
	env, err := ansibleEnv(ws)
	if err != nil {
		return command{}, err
	}
	marker := "\x1edispatch-event:" + rand.Text()[:12] + ":"
	env = append(env,
		"DISPATCH_EVENT_MARKER="+marker,
		// Ours first, then Ansible's defaults.
		"ANSIBLE_CALLBACK_PLUGINS="+pluginDir+":~/.ansible/plugins/callback:/usr/share/ansible/plugins/callback",
		"ANSIBLE_RETRY_FILES_ENABLED=False",
	)
	return command{argv: argv, dir: ws.projectDir, env: env, eventMarker: marker}, nil
}

// ansibleEnv is the environment of the Ansible commands the runner runs:
// the job's, plus settings for running in a job pod and the search paths
// of the project's requirements.
func ansibleEnv(ws *workspace) ([]string, error) {
	tmp := ws.path("ansible", "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	env := append(ws.env[:len(ws.env):len(ws.env)],
		// Job pods start without known hosts.
		"ANSIBLE_HOST_KEY_CHECKING=False",
		// Output is a pipe, not a terminal; the web log renders ANSI colors.
		"ANSIBLE_FORCE_COLOR=True",
		// Writable places, whatever the image's home directory is.
		"ANSIBLE_LOCAL_TEMP="+tmp,
		"ANSIBLE_SSH_CONTROL_PATH_DIR="+ws.path("ansible", "cp"),
		"PYTHONUNBUFFERED=1",
	)
	return append(env, ws.ansibleEnv...), nil
}

// --- Inventory sync jobs -----------------------------------------------------

// inventoryResultFile is where an inventory sync's ansible-inventory
// writes the inventory it found.
func inventoryResultFile(ws *workspace) string { return ws.path("inventory-result.json") }

// inventorySyncCommand runs ansible-inventory on the source (a file or
// directory in the project), in the project directory so the project's
// ansible.cfg applies, writing the inventory to inventoryResultFile.
// --export keeps group variables on their groups instead of merging them
// into every host's. A source Ansible can't parse fails the job rather
// than syncing an empty inventory. Vault credentials decrypt vaulted
// variable files; other credentials reach inventory plugins and scripts
// through the environment (see prepare).
func inventorySyncCommand(spec *runnerapi.RunnerJobSpec, ws *workspace) (command, error) {
	src, err := projectPath(spec, ws, "inventory source", true)
	if err != nil {
		return command{}, err
	}
	argv := []string{"ansible-inventory", "-i", src, "--list", "--export", "--output", inventoryResultFile(ws)}
	if spec.Verbosity > 0 {
		argv = append(argv, "-"+strings.Repeat("v", min(spec.Verbosity, 5)))
	}
	var vaults []runnerapi.RunnerCredential
	for _, c := range spec.Credentials {
		if c.Kind == runnerapi.Vault {
			vaults = append(vaults, c)
		}
	}
	credArgs, err := ansibleCredentialArgs(vaults, ws)
	if err != nil {
		return command{}, err
	}
	argv = append(argv, credArgs...)
	env, err := ansibleEnv(ws)
	if err != nil {
		return command{}, err
	}
	env = append(env,
		"ANSIBLE_INVENTORY_UNPARSED_FAILED=True",
		"ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED=True",
	)
	return command{argv: argv, dir: ws.projectDir, env: env}, nil
}

// staticInventory converts an inventory in the dynamic inventory script
// format (the API's export, and DISPATCH_INVENTORY) to the structure of
// Ansible's YAML inventory plugin, which also reads JSON files:
//
//	{"all": {"vars": …, "hosts": {host: hostvars, …},
//	         "children": {group: {"vars": …, "hosts": {host: null}, "children": {child: null}}}}}
//
// Each host's variables are given once, under all; hosts that are in no
// other group end up in ungrouped, as before.
func staticInventory(export map[string]any) map[string]any {
	asMap := func(v any) map[string]any {
		m, _ := v.(map[string]any)
		return m
	}
	asSet := func(v any) map[string]any {
		out := map[string]any{}
		list, _ := v.([]any)
		for _, x := range list {
			if name, ok := x.(string); ok {
				out[name] = nil
			}
		}
		return out
	}
	hosts := map[string]any{}
	for name, vars := range asMap(asMap(export["_meta"])["hostvars"]) {
		if vars == nil {
			vars = map[string]any{}
		}
		hosts[name] = vars
	}
	all := map[string]any{"hosts": hosts}
	if v := asMap(asMap(export["all"])["vars"]); len(v) > 0 {
		all["vars"] = v
	}
	groups := map[string]any{}
	for name, g := range export {
		if name == "_meta" || name == "all" || name == "ungrouped" {
			continue
		}
		in := asMap(g)
		def := map[string]any{}
		if h := asSet(in["hosts"]); len(h) > 0 {
			def["hosts"] = h
		}
		if v := asMap(in["vars"]); len(v) > 0 {
			def["vars"] = v
		}
		if c := asSet(in["children"]); len(c) > 0 {
			def["children"] = c
		}
		groups[name] = def
	}
	if len(groups) > 0 {
		all["children"] = groups
	}
	return map[string]any{"all": all}
}

// ansibleCredentialArgs writes the credentials ansible-playbook needs to
// files and returns the options that point at them:
//
//   - ssh: -u, --private-key (decrypted) and --become-password-file
//   - username_password: -u, --connection-password-file and
//     --become-password-file
//   - vault: --vault-id or --vault-password-file
//
// Other kinds are available to the playbook through DISPATCH_CREDENTIALS
// and DISPATCH_CREDENTIALS_DIR, like for any job.
func ansibleCredentialArgs(creds []runnerapi.RunnerCredential, ws *workspace) ([]string, error) {
	dir := ws.secret("ansible")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var args []string
	n := 0
	file := func(name, content string) (string, error) {
		n++
		p := filepath.Join(dir, fmt.Sprintf("%d-%s", n, name))
		return p, os.WriteFile(p, []byte(content), 0o600)
	}
	machine := ""
	for _, c := range creds {
		in := c.Inputs
		switch c.Kind {
		case runnerapi.Ssh, runnerapi.UsernamePassword:
			if machine != "" {
				return nil, fmt.Errorf("The job has more than one machine credential (%q and %q)", machine, c.Name)
			}
			machine = c.Name
			if in["username"] != "" {
				args = append(args, "-u", in["username"])
			}
			if in["private_key"] != "" {
				n++
				p := filepath.Join(dir, fmt.Sprintf("%d-ssh_key", n))
				if err := writePrivateKey(p, in["private_key"], in["passphrase"]); err != nil {
					return nil, fmt.Errorf("Credential %q: %w", c.Name, err)
				}
				args = append(args, "--private-key", p)
			}
			if in["password"] != "" {
				p, err := file("connection_password", in["password"])
				if err != nil {
					return nil, err
				}
				args = append(args, "--connection-password-file", p)
			}
			if in["become_password"] != "" {
				p, err := file("become_password", in["become_password"])
				if err != nil {
					return nil, err
				}
				args = append(args, "--become-password-file", p)
			}
		case runnerapi.Vault:
			// Not executable: Ansible would run it as a script.
			p, err := file("vault_password", in["password"])
			if err != nil {
				return nil, err
			}
			if id := in["vault_id"]; id != "" {
				args = append(args, "--vault-id", id+"@"+p)
			} else {
				args = append(args, "--vault-password-file", p)
			}
		}
	}
	return args, nil
}

// parseEventLine turns a line the callback plugin wrote into an event, or
// reports false if the line is not one.
func parseEventLine(line, marker string) (typ, host string, data map[string]any, ok bool) {
	rest, found := strings.CutPrefix(line, marker)
	if marker == "" || !found {
		return "", "", nil, false
	}
	var ev struct {
		Type string         `json:"type"`
		Host *string        `json:"host"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(rest), &ev); err != nil || !eventType.MatchString(ev.Type) {
		return "", "", nil, false
	}
	if ev.Host != nil {
		host = truncateHost(*ev.Host)
	}
	return ev.Type, host, ev.Data, true
}

// truncateHost keeps a host name within the API's limit of 255 bytes.
func truncateHost(h string) string {
	if len(h) <= 255 {
		return h
	}
	return strings.ToValidUTF8(h[:255], "")
}
