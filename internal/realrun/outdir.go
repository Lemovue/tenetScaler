package realrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Names of Run's outputs inside Plan.OutDir.
const (
	runsCSVFile       = "runs.csv"
	summaryJSONFile   = "summary.json"
	summaryMDFile     = "summary.md"
	violationPlotFile = "victim_violation.svg"
	replicasPlotFile  = "timeline_replicas.svg"
	ttftPlotFile      = "timeline_victim_ttft.svg"
	recordsDir        = "records"
	timelineDir       = "timeline"
	decisionsDir      = "decisions"
)

// runOutputs are every file and directory Run writes into OutDir; nothing
// else there (such as the profile's profile.md) is Run's.
var runOutputs = []string{
	runsCSVFile, summaryJSONFile, summaryMDFile, violationPlotFile, replicasPlotFile, ttftPlotFile,
	recordsDir, timelineDir, decisionsDir,
}

// prepareOutDir creates OutDir and makes sure it holds no results of an
// earlier Run, so that the raw files of two experiments are never mixed:
// it refuses when any of runOutputs exists (an empty directory does not
// count) unless p.Overwrite is set, in which case it removes them.
func prepareOutDir(p Plan) error {
	if err := os.MkdirAll(p.OutDir, 0o755); err != nil {
		return fmt.Errorf("realrun: %w", err)
	}
	var found []string
	for _, name := range runOutputs {
		present, err := holdsOutput(filepath.Join(p.OutDir, name))
		if err != nil {
			return fmt.Errorf("realrun: %w", err)
		}
		if present {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		return nil
	}
	if !p.Overwrite {
		return fmt.Errorf("realrun: %s already holds the results of an earlier run (%s); choose another output directory or overwrite them (-overwrite)",
			p.OutDir, strings.Join(found, ", "))
	}
	for _, name := range found {
		if err := os.RemoveAll(filepath.Join(p.OutDir, name)); err != nil {
			return fmt.Errorf("realrun: remove earlier results: %w", err)
		}
	}
	p.Logger.Info("removed the results of an earlier run", "dir", p.OutDir, "removed", found)
	return nil
}

// holdsOutput reports whether path exists and, for a directory, is not
// empty.
func holdsOutput(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !fi.IsDir() {
		return err == nil, err
	}
	d, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer d.Close()
	if _, err := d.Readdirnames(1); errors.Is(err, io.EOF) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}
