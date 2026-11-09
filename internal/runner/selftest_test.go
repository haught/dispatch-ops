package runner

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func fakeSelfTestEnv(uid int, tools ...string) selfTestEnv {
	return selfTestEnv{
		uid: uid,
		lookPath: func(name string) (string, error) {
			if slices.Contains(tools, name) {
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
		collections: func() ([]string, error) {
			return []string{"ansible.posix 2.0.0", "community.general 11.0.0"}, nil
		},
	}
}

func TestSelfTest(t *testing.T) {
	ansible := []string{"git", "ssh", "ansible-playbook", "ansible-inventory", "ansible-galaxy"}
	for _, tc := range []struct {
		name     string
		env      selfTestEnv
		wantCode int
		want     []string
		notWant  []string
	}{
		{
			name: "shell image", env: fakeSelfTestEnv(1000, "git", "ssh"), wantCode: 0,
			want:    []string{"ok    user               uid 1000", "ok    git", "--    ansible", "\nok\n"},
			notWant: []string{"FAIL", "collections"},
		},
		{
			name: "ansible image", env: fakeSelfTestEnv(1000, ansible...), wantCode: 0,
			want: []string{"ok    ansible-inventory", "2 installed:", "community.general 11.0.0"},
		},
		{
			name: "root", env: fakeSelfTestEnv(0, "git", "ssh"), wantCode: 1,
			want: []string{"FAIL  user", "USER 1000:1000", "FAIL: this image"},
		},
		{
			name: "no git in a container-job image", env: fakeSelfTestEnv(1000, "ssh"), wantCode: 0,
			want: []string{"warn  git"},
		},
		{
			name: "no git in an ansible image", env: fakeSelfTestEnv(1000, "ssh", "ansible-playbook", "ansible-inventory", "ansible-galaxy"), wantCode: 1,
			want: []string{"FAIL  git"},
		},
		{
			name: "no ssh is only a warning", env: fakeSelfTestEnv(1000, "git"), wantCode: 0,
			want: []string{"warn  ssh"},
		},
		{
			name: "partial ansible", env: fakeSelfTestEnv(1000, "git", "ssh", "ansible-playbook", "ansible-galaxy"), wantCode: 1,
			want: []string{"FAIL  ansible-inventory"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if code := selfTest(&out, "v1.2.3", tc.env); code != tc.wantCode {
				t.Errorf("exit code %d, want %d\n%s", code, tc.wantCode, out.String())
			}
			got := out.String()
			if !strings.HasPrefix(got, "dispatch-runner v1.2.3, runner protocol ") {
				t.Errorf("header missing:\n%s", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("output lacks %q:\n%s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("output has %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestSelfTestCollectionsError(t *testing.T) {
	env := fakeSelfTestEnv(1000, "git", "ssh", "ansible-playbook", "ansible-inventory", "ansible-galaxy")
	env.collections = func() ([]string, error) { return nil, errors.New("boom") }
	var out strings.Builder
	if code := selfTest(&out, "dev", env); code != 1 || !strings.Contains(out.String(), "FAIL  collections") {
		t.Errorf("code %d\n%s", code, out.String())
	}
}

func TestParseCollectionList(t *testing.T) {
	out := []byte(`{
		"/home/runner/.ansible/collections/ansible_collections": {},
		"/usr/share/ansible/collections/ansible_collections": {
			"community.general": {"version": "11.0.0"},
			"ansible.posix": {"version": "2.0.0"}
		},
		"/opt/ansible/lib/python3.13/site-packages/ansible_collections": {
			"ansible.posix": {"version": "2.0.0"},
			"ansible.utils": {"version": "6.0.0"}
		}
	}`)
	got, err := parseCollectionList(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ansible.posix 2.0.0", "ansible.utils 6.0.0", "community.general 11.0.0"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := parseCollectionList([]byte("not json")); err == nil {
		t.Error("want an error for bad JSON")
	}
}
