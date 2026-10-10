package setup

import (
	"io"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
)

// notOnThePage are config keys the settings page sets another way, or deliberately leaves alone.
var notOnThePage = map[string]string{
	"version":                    "written by config migration",
	"notify.webhook_url_file":    "the page takes the URL itself and writes the file",
	"logs.token_file":            "the page takes the token itself and writes the file",
	"pools.*.type":               "only lxc pools exist so far",
	"targets.*.app_slug":         "comes from the GitHub App",
	"targets.*.org":              "targets are added by connecting a GitHub App",
	"targets.*.user":             "targets are added by connecting a GitHub App",
	"targets.*.app_id":           "comes from the GitHub App",
	"targets.*.installation_id":  "comes from the GitHub App",
	"targets.*.private_key_file": "comes from the GitHub App",
	"targets.*.pools":            "a target field outside the generic list",
	"targets.*.repos":            "a target field outside the generic list",
	"targets.*.runner_group":     "a target field outside the generic list",
}

// yamlKeys lists every setting in the config file as a dotted path, with * for map keys and list items.
// A path the page edits as one value (such as network.allow) is not split further.
func yamlKeys(t reflect.Type, prefix string, whole map[string]bool, out *[]string) {
	if whole[strings.TrimSuffix(prefix, ".")] {
		*out = append(*out, strings.TrimSuffix(prefix, "."))
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "-" || strings.Contains(opts, "inline") || !f.IsExported() {
				continue
			}
			yamlKeys(f.Type, prefix+name+".", whole, out)
		}
		return
	case reflect.Map:
		if e := t.Elem(); e.Kind() == reflect.Pointer || e.Kind() == reflect.Struct {
			yamlKeys(e, prefix+"*.", whole, out)
			return
		}
	case reflect.Slice:
		if e := t.Elem(); e.Kind() == reflect.Struct {
			yamlKeys(e, prefix+"*.", whole, out)
			return
		}
	}
	*out = append(*out, strings.TrimSuffix(prefix, "."))
}

// A setting added to the config without a field on the page would leave it editable only by hand.
func TestEveryConfigSettingIsOnThePage(t *testing.T) {
	onPage := map[string]bool{}
	for _, sec := range hostFields() {
		for _, f := range sec.Fields {
			onPage[f.Name] = true
		}
	}
	for _, f := range poolFields() {
		onPage["pools.*."+f.Key] = true
	}
	var keys []string
	yamlKeys(reflect.TypeOf(config.Config{}), "", onPage, &keys)
	for _, k := range keys {
		if !onPage[k] && notOnThePage[k] == "" {
			t.Errorf("config setting %s has no field on the settings page (internal/setup/fields.go)", k)
		}
		delete(onPage, k)
	}
	for k := range onPage {
		t.Errorf("settings page field %s is not a config setting", k)
	}
}

// changed is a valid value different from the default for each field, so saving proves the field
// reaches the config file and comes back on the page.
var changed = map[string]string{
	"host_id":                    "pve-b",
	"proxmox.node":               "pve2",
	"proxmox.max_runners":        "9",
	"proxmox.vmid_range":         "8000-8099",
	"proxmox.pool":               "ci",
	"proxmox.storage":            "local-zfs",
	"proxmox.min_free_memory_mb": "3072",
	"proxmox.bridge":             "vmbr4",
	"network.blocked":            "192.0.2.0/24, 198.51.100.0/24",
	"network.allow":              "udp/53@192.0.2.1, tcp/443",
	"network.dns":                "192.0.2.53",
	"images.retention_days":      "7",
	"images.repo":                "example-org/images",
	"images.vmid_range":          "9600-9699",
	"images.cache_storage":       "nfs-iso",
	"images.rootfs_gb":           "24",
	"images.check_interval":      "2h",
	"update.channel":             "stable",
	"update.interval":            "3h",
	"update.repo":                "example-org/pve-runners",
	"notify.format":              "slack",
	"logs.url":                   "https://logs.example.com",
	"logs.user":                  "42",
	"logs.interval":              "30s",
	"github.api_url":             "https://github.example.com/api/v3",
	"github.runner_version":      "2.330.0",
	"github.runner_sha256":       strings.Repeat("ab", 32),
	"web.listen":                 "127.0.0.1:9443",
}

var changedPool = map[string]string{
	"image":        "",
	"template":     "9100",
	"mode":         "ephemeral",
	"labels":       "proxmox, big",
	"cores":        "8",
	"memory_mb":    "16384",
	"disk_gb":      "60",
	"max":          "3",
	"min":          "1",
	"max_lifetime": "6h",
	"bridge":       "vmbr5",
	"vlan":         "20",
	"scale_set":    "big-runners",
}

// Every field on the page is changed and saved at once; each must land in the config file and show
// on the page again as it was entered.
func TestEveryFieldSavesAndShowsAgain(t *testing.T) {
	s, srv, saved, path := settingsServer(t)
	c := client(t)
	c.PostForm(srv.URL+"/login", url.Values{"user": {"root@pam"}, "password": {"pw"}})

	before := s.Cfg
	form := browserForm(before)
	want := map[string]string{}
	for _, sec := range hostFields() {
		for _, f := range sec.Fields {
			if f.Kind == "bool" {
				// Flip every checkbox.
				if f.get(before) == "true" {
					form.Del(f.Name)
					want[f.Name] = "false"
				} else {
					form.Set(f.Name, "on")
					want[f.Name] = "true"
				}
				continue
			}
			v, ok := changed[f.Name]
			if !ok {
				t.Errorf("no test value for field %s; add one to changed", f.Name)
				continue
			}
			if v == f.get(before) {
				t.Errorf("test value for %s is its default", f.Name)
			}
			if f.Kind == "select" && !contains(f.Options, v) {
				t.Errorf("test value %q for %s is not one of %v", v, f.Name, f.Options)
			}
			form.Set(f.Name, v)
			want[f.Name] = v
		}
	}
	// Turning network isolation on needs the firewall, which only matters when spawning; saving is fine.
	for _, f := range poolFields() {
		name := "pool.default." + f.Key
		if f.Kind == "bool" {
			if f.get(before.Pools["default"]) == "true" {
				form.Del(name)
				want[name] = "false"
			} else {
				form.Set(name, "on")
				want[name] = "true"
			}
			continue
		}
		v, ok := changedPool[f.Key]
		if !ok {
			t.Errorf("no test value for pool field %s; add one to changedPool", f.Key)
			continue
		}
		form.Set(name, v)
		want[name] = v
	}
	if t.Failed() {
		return
	}
	form.Set("loki_token", "glc_test") // logs.url needs a token
	resp, err := c.PostForm(srv.URL+"/settings", form)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || *saved != 1 {
		t.Fatalf("save: %d, restarts %d, error %q\n%s", resp.StatusCode, *saved, s.errMsg, b)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range hostFields() {
		for _, f := range sec.Fields {
			if v := f.get(got); v != want[f.Name] {
				t.Errorf("%s saved as %q, want %q", f.Name, v, want[f.Name])
			}
		}
	}
	for _, f := range poolFields() {
		if v := f.get(got.Pools["default"]); v != want["pool.default."+f.Key] {
			t.Errorf("pool.default.%s saved as %q, want %q", f.Key, v, want["pool.default."+f.Key])
		}
	}

	// The page after the restart shows what was saved.
	s.Cfg = got
	resp, err = c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	names := make([]string, 0, len(want))
	for k := range want {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := want[k]
		switch v {
		case "true":
			if !hasInput(string(page), k, "checked") {
				t.Errorf("page does not show %s ticked", k)
			}
		case "false":
			if hasInput(string(page), k, "checked") {
				t.Errorf("page shows %s ticked", k)
			}
		default:
			if !strings.Contains(string(page), `name="`+k+`"`) || !strings.Contains(string(page), escape(v)) {
				t.Errorf("page does not show %s = %q", k, v)
			}
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// hasInput reports whether the tag of the input named name contains attr.
func hasInput(page, name, attr string) bool {
	i := strings.Index(page, `name="`+name+`"`)
	if i < 0 {
		return false
	}
	start := strings.LastIndex(page[:i], "<")
	end := strings.Index(page[i:], ">")
	return start >= 0 && end >= 0 && strings.Contains(page[start:i+end], attr)
}

func escape(v string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;", "+", "&#43;")
	return r.Replace(v)
}
