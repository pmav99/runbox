package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

var runDirPattern = regexp.MustCompile(`^run-(\d+)$`)

// listRuns returns the numbers of existing run-N directories, ascending.
func listRuns(base string) ([]int, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var nums []int
	for _, e := range entries {
		m := runDirPattern.FindStringSubmatch(e.Name())
		if m == nil || !e.IsDir() {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	return nums, nil
}

// createRunDir creates base/run-N with N one higher than the highest existing
// run. os.Mkdir is atomic, so if another invocation wins the race for N we
// simply retry with the next number.
func createRunDir(base string) (dir string, n int, err error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", 0, err
	}
	for range 100 {
		nums, err := listRuns(base)
		if err != nil {
			return "", 0, err
		}
		n = 1
		if len(nums) > 0 {
			n = nums[len(nums)-1] + 1
		}
		dir = filepath.Join(base, fmt.Sprintf("run-%d", n))
		err = os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, n, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", 0, err
		}
	}
	return "", 0, errors.New("could not create a run directory: too many concurrent invocations")
}

// updateLatest atomically points base/latest at run-n by creating a temporary
// symlink and renaming it over the old one. The link target is relative so the
// .runbox directory can be moved.
func updateLatest(base string, n int) error {
	tmp := filepath.Join(base, fmt.Sprintf(".latest-%d", os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(fmt.Sprintf("run-%d", n), tmp); err != nil {
		return fmt.Errorf("creating latest symlink: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(base, "latest")); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("updating latest symlink: %w", err)
	}
	return nil
}

// prune removes all but the keep highest-numbered runs. The run numbered
// current is never removed.
func prune(base string, keep, current int) error {
	nums, err := listRuns(base)
	if err != nil {
		return err
	}
	var errs []error
	for i := 0; i < len(nums)-keep; i++ {
		if nums[i] == current {
			continue
		}
		dir := filepath.Join(base, fmt.Sprintf("run-%d", nums[i]))
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
