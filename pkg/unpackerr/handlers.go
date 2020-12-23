package unpackerr

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"golift.io/starr"
	"golift.io/xtractr"
)

// Extract holds data for files being extracted.
type Extract struct {
	Path    string
	App     string
	IDs     map[string]interface{}
	Status  ExtractStatus
	Updated time.Time
	Resp    *xtractr.Response
	Retries uint
}

// checkQueueChanges checks each item for state changes from the app queues.
func (u *Unpackerr) checkQueueChanges() {
	for name, data := range u.Map {
		switch {
		case data.App == FolderString:
			continue // folders are handled in folder.go.
		case !u.haveQitem(name, data.App):
			// This fires when an items becomes missing (imported) from the application queue.
			switch elapsed := time.Since(data.Updated); {
			case data.Status == WAITING:
				// A waiting item just fell out of the queue. We never extracted it. Remove it and move on.
				delete(u.Map, name)
				u.Printf("[%v] Imported: %v (not extracted, removing from history)", data.App, name)
			case data.Status > IMPORTED:
				u.Debugf("Already imported? %s", name)
			case data.Status == IMPORTED:
				u.Debugf("%v: Awaiting Delete Delay (%v remains): %v",
					data.App, u.DeleteDelay.Duration-elapsed.Round(time.Second), name)
			default:
				u.updateQueueStatus(&newStatus{Name: name, Status: IMPORTED, Resp: data.Resp})
				u.Printf("[%v] Imported: %v (delete in %v)", data.App, name, u.DeleteDelay)
			}
		case data.Status == IMPORTED:
			// The item fell out of the app queue and came back. Reset it.
			u.Printf("%s: Extraction Not Imported: %s - De-queued and returned.", data.App, name)
			data.Status = EXTRACTED
		case data.Status > IMPORTED:
			// The item fell out of the app queue and came back. Reset it.
			u.Printf("%s: Extraction Restarting: %s - Deleted Item De-queued and returned.", data.App, name)
			data.Status = WAITING
			data.Updated = time.Now()
		}

		u.Debugf("%s: Status: %s (%v, elapsed: %v)", data.App, name, data.Status.Desc(),
			time.Since(data.Updated).Round(time.Second))
	}
}

// handleCompletedDownload checks if a sonarr/radarr/lidar completed item needs to be extracted.
// This is called from the app methods.
func (u *Unpackerr) handleCompletedDownload(name, app, path string, ids map[string]interface{}) {
	item, ok := u.Map[name]
	if !ok {
		u.Map[name] = &Extract{
			App:     app,
			Path:    path,
			IDs:     ids,
			Status:  WAITING,
			Updated: time.Now(),
		}
		item = u.Map[name]
	}

	if time.Since(item.Updated) < u.Config.StartDelay.Duration {
		u.Printf("[%s] Waiting for Start Delay: %v (%v remains)", app, name,
			u.Config.StartDelay.Duration-time.Since(item.Updated).Round(time.Second))

		return
	}

	files := xtractr.FindCompressedFiles(path)
	if len(files) == 0 {
		_, err := os.Stat(path)
		u.Printf("[%s] Completed item still waiting: %s, no extractable files found at: %s (stat err: %v)",
			app, name, path, err)

		return
	}

	item.Status = QUEUED
	item.Updated = time.Now()

	queueSize, _ := u.Extract(&xtractr.Xtract{
		Name:       name,
		SearchPath: path,
		TempFolder: false,
		DeleteOrig: false,
		CBChannel:  u.updates,
	})
	u.Printf("[%s] Extraction Queued: %s, extractable files: %d, items in queue: %d", app, path, len(files), queueSize)
}

// checkExtractDone checks if an extracted item imported items needs to be deleted.
// Or if an extraction failed and needs to be restarted.
// This runs at a short interval to check for extraction state changes, and shuold return quickly.
func (u *Unpackerr) checkExtractDone() {
	for name, data := range u.Map {
		switch elapsed := time.Since(data.Updated); {
		case data.Status == DELETED && elapsed >= u.DeleteDelay.Duration:
			// Remove the item from history some time after it's deleted.
			u.Finished++
			delete(u.Map, name)
			u.Printf("[%s] Finished, Removed History: %v", data.App, name)
		case data.App == FolderString:
			continue // folders are handled in folder.go.
		case data.Status == EXTRACTFAILED && elapsed >= u.RetryDelay.Duration &&
			(u.MaxRetries == 0 || data.Retries < u.MaxRetries):
			u.Retries++
			data.Retries++
			data.Status = WAITING
			data.Updated = time.Now()
			u.Printf("[%s] Extract failed %v ago, triggering restart (%d/%d): %v",
				data.App, elapsed.Round(time.Second), data.Retries, u.MaxRetries, name)
		case data.Status == IMPORTED && elapsed >= u.DeleteDelay.Duration:
			if len(data.Resp.NewFiles) > 0 {
				// In a routine so it can run slowly and not block.
				go u.DeleteFiles(data.Resp.NewFiles...)
			}

			u.updateQueueStatus(&newStatus{Name: name, Status: DELETED, Resp: data.Resp})
		}
	}
}

// handleXtractrCallback handles callbacks from the xtractr library for sonarr/radarr/lidarr.
// This takes the provided info and logs it then sends it the queue update method.
func (u *Unpackerr) handleXtractrCallback(resp *xtractr.Response) {
	switch {
	case !resp.Done:
		u.Printf("Extraction Started: %s, items in queue: %d", resp.X.Name, resp.Queued)
		u.updateQueueStatus(&newStatus{Name: resp.X.Name, Status: EXTRACTING, Resp: resp})
	case resp.Error != nil:
		u.Printf("Extraction Error: %s: %v", resp.X.Name, resp.Error)
		u.updateQueueStatus(&newStatus{Name: resp.X.Name, Status: EXTRACTFAILED, Resp: resp})
	default:
		u.Printf("Extraction Finished: %s => elapsed: %v, archives: %d, extra archives: %d, "+
			"files extracted: %d, wrote: %dMiB", resp.X.Name, resp.Elapsed.Round(time.Second),
			len(resp.Archives), len(resp.Extras), len(resp.NewFiles), resp.Size/mebiByte)
		u.updateQueueStatus(&newStatus{Name: resp.X.Name, Status: EXTRACTED, Resp: resp})
	}
}

// Looking for a message that looks like:
// "No files found are eligible for import in /downloads/Downloading/Space.Warriors.S99E88.GrOuP.1080p.WEB.x264".
func (u *Unpackerr) getDownloadPath(s []starr.StatusMessage, app, title string, paths []string) string {
	var errs []error

	for _, path := range paths {
		path = filepath.Join(path, title)

		switch _, err := os.Stat(path); err { // nolint: errorlint
		default:
			errs = append(errs, err)
		case nil:
			return path
		}
	}

	defer u.Debugf("%s: Errors encountered looking for %s path: %q", app, title, errs)

	// The following code tries to find the path in the queued item's error message.
	for _, m := range s {
		if m.Title != title {
			continue
		}

		for _, msg := range m.Messages {
			if strings.HasPrefix(msg, prefixPathMsg) && strings.HasSuffix(msg, title) {
				path := strings.TrimSpace(strings.TrimPrefix(msg, prefixPathMsg))
				u.Debugf("%s: Configured paths do not exist; trying path found in status message: %s", app, path)

				return path
			}
		}
	}

	u.Debugf("%s: Configured paths do not exist; could not find alternative path in error message for %s", app, title)

	return filepath.Join(paths[0], title) // useless, but return something. :(
}

// isComplete is run so many times in different places that is became a method.
func (u *Unpackerr) isComplete(status, protocol, protos string) bool {
	for _, s := range strings.Fields(strings.ReplaceAll(protos, ",", " ")) {
		if strings.EqualFold(protocol, s) {
			return strings.EqualFold(status, "completed")
		}
	}

	return false
}
