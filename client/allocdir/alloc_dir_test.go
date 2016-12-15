package allocdir

import (
	"archive/tar"
	"bytes"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	tomb "gopkg.in/tomb.v1"

	"github.com/hashicorp/nomad/client/testutil"
	"github.com/hashicorp/nomad/nomad/structs"
)

var (
	osMountSharedDirSupport = map[string]bool{
		"darwin": true,
		"linux":  true,
	}

	t1 = &structs.Task{
		Name:   "web",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/date",
			"args":    "+%s",
		},
		Resources: &structs.Resources{
			DiskMB: 1,
		},
	}

	t2 = &structs.Task{
		Name:   "web2",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/date",
			"args":    "+%s",
		},
		Resources: &structs.Resources{
			DiskMB: 1,
		},
	}
)

// Test that AllocDir.Build builds just the alloc directory.
func TestAllocDir_BuildAlloc(t *testing.T) {
	tmp, err := ioutil.TempDir("", "AllocDir")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	d := NewAllocDir(tmp)
	defer d.Destroy()
	d.NewTaskDir(t1.Name)
	d.NewTaskDir(t2.Name)
	if err := d.Build(); err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	// Check that the AllocDir and each of the task directories exist.
	if _, err := os.Stat(d.AllocDir); os.IsNotExist(err) {
		t.Fatalf("Build() didn't create AllocDir %v", d.AllocDir)
	}

	for _, task := range []*structs.Task{t1, t2} {
		tDir, ok := d.TaskDirs[task.Name]
		if !ok {
			t.Fatalf("Task directory not found for %v", task.Name)
		}

		if stat, _ := os.Stat(tDir.Dir); stat != nil {
			t.Fatalf("Build() created TaskDir %v", tDir.Dir)
		}

		if stat, _ := os.Stat(tDir.SecretsDir); stat != nil {
			t.Fatalf("Build() created secret dir %v", tDir.Dir)
		}
	}
}

func TestAllocDir_EmbedDirs(t *testing.T) {
	tmp, err := ioutil.TempDir("", "AllocDir")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	d := NewAllocDir(tmp)
	defer d.Destroy()
	tasks := []*structs.Task{t1, t2}
	if err := d.Build(tasks); err != nil {
		t.Fatalf("Build(%v) failed: %v", tasks, err)
	}

	// Create a fake host directory, with a file, and a subfolder that contains
	// a file.
	host, err := ioutil.TempDir("", "AllocDirHost")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(host)

	subDirName := "subdir"
	subDir := filepath.Join(host, subDirName)
	if err := os.MkdirAll(subDir, 0777); err != nil {
		t.Fatalf("Failed to make subdir %v: %v", subDir, err)
	}

	file := "foo"
	subFile := "bar"
	if err := ioutil.WriteFile(filepath.Join(host, file), []byte{'a'}, 0777); err != nil {
		t.Fatalf("Coudn't create file in host dir %v: %v", host, err)
	}

	if err := ioutil.WriteFile(filepath.Join(subDir, subFile), []byte{'a'}, 0777); err != nil {
		t.Fatalf("Coudn't create file in host subdir %v: %v", subDir, err)
	}

	// Create mapping from host dir to task dir.
	task := tasks[0].Name
	taskDest := "bin/test/"
	mapping := map[string]string{host: taskDest}
	if err := d.Embed(task, mapping); err != nil {
		t.Fatalf("Embed(%v, %v) failed: %v", task, mapping, err)
	}

	// Check that the embedding was done properly.
	taskDir, ok := d.TaskDirs[task]
	if !ok {
		t.Fatalf("Task directory not found for %v", task)
	}

	exp := []string{filepath.Join(taskDir, taskDest, file), filepath.Join(taskDir, taskDest, subDirName, subFile)}
	for _, e := range exp {
		if _, err := os.Stat(e); os.IsNotExist(err) {
			t.Fatalf("File %v not embeded: %v", e, err)
		}
	}
}

func TestAllocDir_MountSharedAlloc(t *testing.T) {
	testutil.MountCompatible(t)
	tmp, err := ioutil.TempDir("", "AllocDir")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	d := NewAllocDir(tmp)
	defer d.Destroy()
	tasks := []*structs.Task{t1, t2}
	if err := d.Build(tasks); err != nil {
		t.Fatalf("Build(%v) failed: %v", tasks, err)
	}

	// Write a file to the shared dir.
	exp := []byte{'f', 'o', 'o'}
	file := "bar"
	if err := ioutil.WriteFile(filepath.Join(d.SharedDir, file), exp, 0777); err != nil {
		t.Fatalf("Couldn't write file to shared directory: %v", err)
	}

	for _, task := range tasks {
		// Mount and then check that the file exists in the task directory.
		if err := d.MountSharedDir(task.Name); err != nil {
			if v, ok := osMountSharedDirSupport[runtime.GOOS]; v && ok {
				t.Fatalf("MountSharedDir(%v) failed: %v", task.Name, err)
			} else {
				t.Skipf("MountShareDir(%v) failed, no OS support")
			}
		}

		taskDir, ok := d.TaskDirs[task.Name]
		if !ok {
			t.Fatalf("Task directory not found for %v", task.Name)
		}

		taskFile := filepath.Join(taskDir, SharedAllocName, file)
		act, err := ioutil.ReadFile(taskFile)
		if err != nil {
			t.Fatalf("Failed to read shared alloc file from task dir: %v", err)
		}

		if !reflect.DeepEqual(act, exp) {
			t.Fatalf("Incorrect data read from task dir: want %v; got %v", exp, act)
		}
	}
}

func TestAllocDir_Snapshot(t *testing.T) {
	tmp, err := ioutil.TempDir("", "AllocDir")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	d := NewAllocDir(tmp)
	defer d.Destroy()

	tasks := []*structs.Task{t1, t2}
	if err := d.Build(tasks); err != nil {
		t.Fatalf("Build(%v) failed: %v", tasks, err)
	}

	dataDir := filepath.Join(d.SharedDir, "data")
	taskDir := d.TaskDirs[t1.Name]
	taskLocal := filepath.Join(taskDir, "local")

	// Write a file to the shared dir.
	exp := []byte{'f', 'o', 'o'}
	file := "bar"
	if err := ioutil.WriteFile(filepath.Join(dataDir, file), exp, 0777); err != nil {
		t.Fatalf("Couldn't write file to shared directory: %v", err)
	}

	// Write a file to the task local
	exp = []byte{'b', 'a', 'r'}
	file1 := "lol"
	if err := ioutil.WriteFile(filepath.Join(taskLocal, file1), exp, 0777); err != nil {
		t.Fatalf("couldn't write to task local directory: %v", err)
	}

	var b bytes.Buffer
	if err := d.Snapshot(&b); err != nil {
		t.Fatalf("err: %v", err)
	}

	tr := tar.NewReader(&b)
	var files []string
	for {
		hdr, err := tr.Next()
		if err != nil && err != io.EOF {
			t.Fatalf("err: %v", err)
		}
		if err == io.EOF {
			break
		}
		if hdr.Typeflag == tar.TypeReg {
			files = append(files, hdr.FileInfo().Name())
		}
	}

	if len(files) != 2 {
		t.Fatalf("bad files: %#v", files)
	}
}

func TestAllocDir_Move(t *testing.T) {
	tmp1, err := ioutil.TempDir("", "AllocDir")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp1)

	tmp2, err := ioutil.TempDir("", "AllocDir")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp2)

	// Create two alloc dirs
	d1 := NewAllocDir(tmp1)
	defer d1.Destroy()

	d2 := NewAllocDir(tmp2)
	defer d2.Destroy()

	tasks := []*structs.Task{t1, t2}
	if err := d1.Build(tasks); err != nil {
		t.Fatalf("Build(%v) failed: %v", tasks, err)
	}

	if err := d2.Build(tasks); err != nil {
		t.Fatalf("Build(%v) failed: %v", tasks, err)
	}

	dataDir := filepath.Join(d1.SharedDir, "data")
	taskDir := d1.TaskDirs[t1.Name]
	taskLocal := filepath.Join(taskDir, "local")

	// Write a file to the shared dir.
	exp := []byte{'f', 'o', 'o'}
	file := "bar"
	if err := ioutil.WriteFile(filepath.Join(dataDir, file), exp, 0777); err != nil {
		t.Fatalf("Couldn't write file to shared directory: %v", err)
	}

	// Write a file to the task local
	exp = []byte{'b', 'a', 'r'}
	file1 := "lol"
	if err := ioutil.WriteFile(filepath.Join(taskLocal, file1), exp, 0777); err != nil {
		t.Fatalf("couldn't write to task local directory: %v", err)
	}

	// Move the d1 allocdir to d2
	if err := d2.Move(d1, []*structs.Task{t1, t2}); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure the files in d1 are present in d2
	fi, err := os.Stat(filepath.Join(d2.SharedDir, "data", "bar"))
	if err != nil || fi == nil {
		t.Fatalf("data dir was not moved")
	}

	fi, err = os.Stat(filepath.Join(d2.TaskDirs[t1.Name], "local", "lol"))
	if err != nil || fi == nil {
		t.Fatalf("task local dir was not moved")
	}
}

func TestAllocDir_EscapeChecking(t *testing.T) {
	tmp, err := ioutil.TempDir("", "AllocDir")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	d := NewAllocDir(tmp)
	defer d.Destroy()
	tasks := []*structs.Task{t1, t2}
	if err := d.Build(tasks); err != nil {
		t.Fatalf("Build(%v) failed: %v", tasks, err)
	}

	// Check that issuing calls that escape the alloc dir returns errors
	// List
	if _, err := d.List(".."); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("List of escaping path didn't error: %v", err)
	}

	// Stat
	if _, err := d.Stat("../foo"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("Stat of escaping path didn't error: %v", err)
	}

	// ReadAt
	if _, err := d.ReadAt("../foo", 0); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("ReadAt of escaping path didn't error: %v", err)
	}

	// BlockUntilExists
	tomb := tomb.Tomb{}
	if _, err := d.BlockUntilExists("../foo", &tomb); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("BlockUntilExists of escaping path didn't error: %v", err)
	}

	// ChangeEvents
	if _, err := d.ChangeEvents("../foo", 0, &tomb); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("ChangeEvents of escaping path didn't error: %v", err)
	}
}

func TestAllocDir_ReadAt_SecretDir(t *testing.T) {
	tmp, err := ioutil.TempDir("", "AllocDir")
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	d := NewAllocDir(tmp)
	defer d.Destroy()
	tasks := []*structs.Task{t1, t2}
	if err := d.Build(tasks); err != nil {
		t.Fatalf("Build(%v) failed: %v", tasks, err)
	}

	// ReadAt of secret dir should fail
	secret := filepath.Join(t1.Name, TaskSecrets, "test_file")
	if _, err := d.ReadAt(secret, 0); err == nil || !strings.Contains(err.Error(), "secret file prohibited") {
		t.Fatalf("ReadAt of secret file didn't error: %v", err)
	}
}

func TestAllocDir_SplitPath(t *testing.T) {
	dir, err := ioutil.TempDir("/tmp", "tmpdirtest")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	dest := filepath.Join(dir, "/foo/bar/baz")
	if err := os.MkdirAll(dest, os.ModePerm); err != nil {
		t.Fatalf("err: %v", err)
	}

	d := NewAllocDir(dir)
	defer d.Destroy()

	info, err := d.splitPath(dest)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(info) != 6 {
		t.Fatalf("expected: %v, actual: %v", 6, len(info))
	}
}

func TestAllocDir_CreateDir(t *testing.T) {
	dir, err := ioutil.TempDir("/tmp", "tmpdirtest")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer os.RemoveAll(dir)

	// create a subdir and a file
	subdir := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(subdir, 0760); err != nil {
		t.Fatalf("err: %v", err)
	}
	subdirMode, err := os.Stat(subdir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Create the above hierarchy under another destination
	dir1, err := ioutil.TempDir("/tmp", "tempdirdest")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	d := NewAllocDir(dir)
	defer d.Destroy()

	if err := d.createDir(dir1, subdir); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure that the subdir had the right perm
	fi, err := os.Stat(filepath.Join(dir1, dir, "subdir"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fi.Mode() != subdirMode.Mode() {
		t.Fatalf("wrong file mode: %v, expected: %v", fi.Mode(), subdirMode.Mode())
	}
}
