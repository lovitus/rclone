package vfscache

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/lib/file"
	"github.com/rclone/rclone/lib/ranges"
)

// Item is stored in the item map
//
// These are written to the backing store to store status
type Item struct {
	mu         sync.Mutex  // protect the variables
	c          *Cache      // cache this is part of
	name       string      // name in the VFS
	opens      int         // number of times file is open
	downloader *downloader // if the file is being downloaded to cache
	o          fs.Object   // object we are caching - may be nil
	fd         *os.File    // handle we are using to read and write to the file
	changed    bool        // set if the item is modified

	// These variables are persisted to backing store
	ATime       time.Time     // last time file was accessed
	Size        int64         // size of the cached item
	Rs          ranges.Ranges // which parts of the file are present
	Fingerprint string        // fingerprint of remote object
}

// StoreFn is called back with an object after it has been uploaded
type StoreFn func(fs.Object)

// newItem returns an item for the cache
func newItem(c *Cache, name string) (item *Item) {
	item = &Item{
		c:     c,
		name:  name,
		ATime: time.Now(),
	}

	// check the cache file exists
	osPath := c.toOSPath(name)
	fi, statErr := os.Stat(osPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			item._removeMeta("cache file doesn't exist")
		} else {
			item._remove(fmt.Sprintf("failed to stat cache file: %v", statErr))
		}
	}

	// Try to load the metadata
	exists, err := item.load()
	if !exists {
		item._removeFile("metadata doesn't exist")
	} else if err != nil {
		item._remove(fmt.Sprintf("failed to load metadata: %v", err))
	}

	// FIXME need to know the size from the Object not from the File
	// FIXME need read/write intent...
	if statErr == nil {
		item.Size = fi.Size()
	}
	return item
}

// clean the item after its cache file has been deleted
func (item *Item) clean() {
	item.Rs = nil
	item.Fingerprint = ""
	item.Size = 0
	item.ATime = time.Now()
}

// load reads an item from the disk or returns nil if not found
func (item *Item) load() (exists bool, err error) {
	item.mu.Lock()
	defer item.mu.Unlock()
	osPathMeta := item.c.toOSPathMeta(item.name)
	in, err := os.Open(osPathMeta)
	if err != nil {
		if os.IsNotExist(err) {
			return false, err
		}
		return true, errors.Wrap(err, "vfs cache item: failed to read metadata")
	}
	defer fs.CheckClose(in, &err)
	decoder := json.NewDecoder(in)
	err = decoder.Decode(&item)
	if err != nil {
		return true, errors.Wrap(err, "vfs cache item: corrupt metadata")
	}
	return true, nil
}

// save writes an item to the disk
//
// call with the lock held
func (item *Item) _save() (err error) {
	osPathMeta := item.c.toOSPathMeta(item.name)
	out, err := os.Create(osPathMeta)
	if err != nil {
		return errors.Wrap(err, "vfs cache item: failed to write metadata")
	}
	defer fs.CheckClose(out, &err)
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "\t")
	err = encoder.Encode(item)
	if err != nil {
		return errors.Wrap(err, "vfs cache item: failed to encode metadata")
	}
	return nil
}

// truncate the item to the given size, creating it if necessary
//
// this does not mark the object as dirty
func (item *Item) truncate(size int64) (err error) {
	if size < 0 {
		// FIXME ignore unknown length files
		return nil
	}

	// Use open handle if available
	fd := item.fd
	if fd == nil {
		osPath := item.c.toOSPath(item.name)
		fd, err = file.OpenFile(osPath, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return errors.Wrap(err, "vfs item truncate: failed to open cache file")
		}

		defer fs.CheckClose(fd, &err)

		err = file.SetSparse(fd)
		if err != nil {
			fs.Debugf(item.name, "vfs item truncate: failed to set as a sparse file: %v", err)
		}
	}

	fs.Debugf(item.name, "vfs cache: truncate to size=%d", size)

	err = fd.Truncate(size)
	if err != nil {
		return errors.Wrap(err, "vfs truncate: failed to truncate")
	}

	return nil
}

// Truncate the item to the current size, creating if necessary
//
// This does not mark the object as dirty
func (item *Item) truncateToCurrentSize() (err error) {
	size, err := item.GetSize()
	if err != nil && !os.IsNotExist(errors.Cause(err)) {
		return errors.Wrap(err, "truncate to current size")
	}
	if size < 0 {
		// FIXME ignore unknown length files
		return nil
	}
	return item.Truncate(size)
}

// Truncate the item to the given size, creating it if necessary
//
// If the new size is shorter than the existing size then the object
// will be shortened and marked as dirty.
//
// If the new size is longer than the old size then the object will be
// extended and the extended data will be filled with zeros. The
// object will be marked as dirty in this case also.
func (item *Item) Truncate(size int64) (err error) {
	// Read old size
	oldSize, err := item.GetSize()
	if err != nil {
		if !os.IsNotExist(errors.Cause(err)) {
			return errors.Wrap(err, "truncate failed to read size")
		}
		oldSize = 0
	}

	err = item.truncate(size)
	if err != nil {
		return err
	}

	item.Size = size
	if size > oldSize {
		// Truncate extends the file in which case all new bytes are
		// read as zeros. In this case we must show we have written to
		// the new parts of the file.
		item._written(oldSize, size)
		item.changed = true
	} else if size < oldSize {
		// Truncate shrinks the file so clip the downloaded ranges
		item.Rs = item.Rs.Intersection(ranges.Range{Pos: 0, Size: size})
		item.changed = true
	} else {
		item.changed = item.o == nil
	}

	return nil
}

// _getSize gets the current size of the item
//
// Call with mutex held
func (item *Item) _getSize() (size int64, err error) {
	var fi os.FileInfo
	if item.fd != nil {
		fi, err = item.fd.Stat()
	} else {
		osPath := item.c.toOSPath(item.name)
		fi, err = os.Stat(osPath)
	}
	if os.IsNotExist(err) {
		if item.o != nil {
			return item.o.Size(), nil
		}
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), err
}

// GetSize gets the current size of the item
func (item *Item) GetSize() (size int64, err error) {
	item.mu.Lock()
	defer item.mu.Unlock()
	return item._getSize()
}

// Exists returns whether the backing file for the item exists or not
func (item *Item) Exists() bool {
	_, err := item.GetSize()
	return err == nil
}

// Open the local file from the object passed in (which may be nil)
// which implies we are about to create the file
func (item *Item) Open(o fs.Object) (err error) {
	defer log.Trace(o, "item=%p", item)("err=%v", &err)
	// FIXME locking
	// item.mu.Lock()
	// defer item.mu.Unlock()

	item.ATime = time.Now()
	item.opens++

	osPath, err := item.c.mkdir(item.name)
	if err != nil {
		return errors.Wrap(err, "vfs cache item: open mkdir failed")
	}

	item.checkObject(o)

	err = item.truncateToCurrentSize()
	if err != nil {
		return errors.Wrap(err, "vfs cache item: open truncate failed")
	}

	if item.opens != 1 {
		return nil
	}
	if item.fd != nil {
		return errors.New("vfs cache item: internal error: didn't Close file")
	}

	fd, err := file.OpenFile(osPath, os.O_RDWR, 0600)
	if err != nil {
		return errors.Wrap(err, "vfs cache item: open failed")
	}
	err = file.SetSparse(fd)
	if err != nil {
		fs.Debugf(item.name, "vfs cache item: failed to set as a sparse file: %v", err)
	}
	item.fd = fd

	err = item._save()
	if err != nil {
		return err
	}

	// Ensure this item is in the cache. It is possible a cache
	// expiry has run and removed the item if it had no opens so
	// we put it back here. If there was an item with opens
	// already then return an error. This shouldn't happen because
	// there should only be one vfs.File with a pointer to this
	// item in at a time.
	oldItem := item.c.put(item.name, item)
	if oldItem != nil {
		oldItem.mu.Lock()
		if oldItem.opens != 0 {
			// Put the item back and return an error
			item.c.put(item.name, oldItem)
			err = errors.Errorf("internal error: item %q already open in the cache", item.name)
		}
		oldItem.mu.Unlock()
	}

	return err
}

// Store stores the local cache file to the remote object, returning
// the new remote object. objOld is the old object if known.
//
// call with item lock held
func (item *Item) _store() (err error) {
	defer log.Trace(item.name, "item=%p", item)("err=%v", &err)
	ctx := context.Background()

	// Ensure any segments not transferred are brought in
	err = item._ensure(0, 0x7fffffffffffffff) // FIXME Size?
	if err != nil {
		return errors.Wrap(err, "vfs cache: failed to download missing parts of cache file")
	}

	// Transfer the temp file to the remote
	cacheObj, err := item.c.fcache.NewObject(ctx, item.name)
	if err != nil {
		return errors.Wrap(err, "vfs cache: failed to find cache file")
	}
	// FIXME why?
	// if objOld != nil {
	// 	remote = objOld.Remote() // use the path of the actual object if available
	// }
	o, err := copyObj(item.c.fremote, item.o, item.name, cacheObj)
	if err != nil {
		return errors.Wrap(err, "vfs cache: failed to transfer file from cache to remote")
	}
	item.o = o
	return nil
}

// Close the cache file
func (item *Item) Close(storeFn StoreFn) (err error) {
	defer log.Trace(item.o, "")("err=%v", &err)
	var downloader *downloader
	// close downloader and set item with mutex unlocked
	defer func() {
		if downloader != nil {
			closeErr := downloader.close(nil)
			if closeErr != nil && err == nil {
				err = closeErr
			}
		}
		if err == nil && storeFn != nil {
			// Write the object back to the VFS layer
			storeFn(item.o)
		}
	}()
	item.mu.Lock()
	defer item.mu.Unlock()

	item.ATime = time.Now()
	item.opens--

	if item.opens < 0 {
		return os.ErrClosed
	} else if item.opens > 0 {
		return nil
	}

	// Update the size on close
	size, err := item._getSize()
	if err == nil {
		item.Size = size
	}
	err = item._save()
	if err != nil {
		return errors.Wrap(err, "close failed to save item")
	}

	// close the downloader
	downloader = item.downloader
	item.downloader = nil

	// close the file handle
	if item.fd == nil {
		return errors.New("vfs cache item: internal error: didn't Open file")
	}
	err = item.fd.Close()
	item.fd = nil

	// if the item hasn't been changed but has been completed then
	// set the modtime from the object
	if !item.changed && item._present() && item.o != nil {
		item._setModTime(item.o.ModTime(context.Background()))
	}

	// upload the file to backing store if changed
	if item.changed {
		err = item._store()
		if err != nil {
			fs.Errorf(item.name, "%v", err)
			return err
		}
		fs.Debugf(item.o, "transferred to remote")
		item.changed = false
	}

	return err
}

// check the fingerprint of an object and update the item or delete
// the cached file accordingly
func (item *Item) checkObject(o fs.Object) {
	item.mu.Lock()
	defer item.mu.Unlock()

	if o == nil {
		if item.Fingerprint != "" {
			// no remote object && local object
			// remove local object
			item._remove("stale (remote deleted)")
		} else {
			// no remote object && no local object
			// OK
		}
	} else {
		remoteFingerprint := item.c.objectFingerprint(o)
		fs.Debugf(item.name, "vfs cache: checking remote fingerprint %q against cached fingerprint %q", remoteFingerprint, item.Fingerprint)
		if item.Fingerprint != "" {
			// remote object && local object
			if remoteFingerprint != item.Fingerprint {
				fs.Debugf(item.name, "vfs cache: removing cached entry as stale (remote fingerprint %q != cached fingerprint %q)", remoteFingerprint, item.Fingerprint)
				item._remove("stale (remote is different)")
			}
		} else {
			// remote object && no local object
			// Set fingerprint
			item.Fingerprint = remoteFingerprint
		}
		item.Size = o.Size()
	}
	item.o = o

}

// remove the cached file
//
// call with lock held
func (item *Item) _removeFile(reason string) {
	osPath := item.c.toOSPath(item.name)
	err := os.Remove(osPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fs.Errorf(item.name, "Failed to remove cache file as %s: %v", reason, err)
		}
	} else {
		fs.Infof(item.name, "Removed cache file as %s", reason)
	}
}

// remove the metadata
//
// call with lock held
func (item *Item) _removeMeta(reason string) {
	osPathMeta := item.c.toOSPathMeta(item.name)
	err := os.Remove(osPathMeta)
	if err != nil {
		if !os.IsNotExist(err) {
			fs.Errorf(item.name, "Failed to remove metadata from cache as %s: %v", reason, err)
		}
	} else {
		fs.Infof(item.name, "Removed metadata from cache as %s", reason)
	}
}

// remove the cached file and empty the metadata
//
// call with lock held
func (item *Item) _remove(reason string) {
	item.clean()
	item._removeFile(reason)
	item._removeMeta(reason)
}

// remove the cached file and empty the metadata
func (item *Item) remove(reason string) {
	item.mu.Lock()
	item._remove(reason)
	item.mu.Unlock()
}

// create a downloader for the item
//
// call with item mutex held
func (item *Item) _newDownloader() (err error) {
	// If no cached object then can't download
	if item.o == nil {
		return errors.New("vfs cache: internal error: tried to download nil object")
	}
	// If downloading the object already stop the downloader and restart it
	if item.downloader != nil {
		_ = item.downloader.close(nil)
		item.downloader = nil
	}
	item.downloader, err = newDownloader(item, item.c.fremote, item.name, item.o)
	return err
}

// _present returns true if the whole file has been downloaded
//
// call with the lock held
func (item *Item) _present() bool {
	if item.downloader != nil && item.downloader.running() {
		return false
	}
	return item.Rs.Present(ranges.Range{Pos: 0, Size: item.Size})
}

// Present returns true if the whole file has been downloaded
func (item *Item) Present() bool {
	item.mu.Lock()
	defer item.mu.Unlock()
	return item._present()
}

// ensure the range from offset, size is present
//
// call with the item lock held
func (item *Item) _ensure(offset, size int64) (err error) {
	defer log.Trace(item.name, "offset=%d, size=%d", offset, size)("err=%v", &err)
	if offset+size > item.Size {
		size = item.Size - offset
	}
	r := ranges.Range{Pos: offset, Size: size}
	present := item.Rs.Present(r)
	downloader := item.downloader
	fs.Debugf(nil, "looking for range=%+v in %+v - present %v", r, item.Rs, present)
	if present {
		return nil
	}
	// FIXME pass in offset here to decide to seek?
	err = item._newDownloader()
	if err != nil {
		return errors.Wrap(err, "Ensure: failed to start downloader")
	}
	downloader = item.downloader
	if downloader == nil {
		return errors.New("internal error: downloader is nil")
	}
	if !downloader.running() {
		// FIXME need to make sure we start in the correct place because some of offset,size might exist
		// FIXME this could stop an old download
		err = downloader.start(offset)
		if err != nil {
			return errors.Wrap(err, "Ensure: failed to run downloader")
		}
	}
	item.mu.Unlock()
	defer item.mu.Lock()
	return item.downloader.ensure(r)
}

// _written shows the range from offset, size is now present
//
// call with lock held
func (item *Item) _written(offset, size int64) {
	defer log.Trace(item.name, "offset=%d, size=%d", offset, size)("")
	item.Rs.Insert(ranges.Range{Pos: offset, Size: offset + size})
	_ = item._save() // FIXME TOO much writing? - mark as modified???
}

// update the fingerprint of the object if any
//
// call with lock held
func (item *Item) _updateFingerprint() {
	if item.o != nil {
		item.Fingerprint = item.c.objectFingerprint(item.o)
		fs.Debugf(item.o, "fingerprint now %q", item.Fingerprint)
	}
}

// setModTime of the cache file
//
// call with lock held
func (item *Item) _setModTime(modTime time.Time) {
	osPath := item.c.toOSPath(item.name)
	err := os.Chtimes(osPath, modTime, modTime)
	if err != nil {
		fs.Errorf(item.name, "Failed to set modification time of cached file: %v", err)
	}
}

// setModTime of the cache file and in the Item
func (item *Item) setModTime(modTime time.Time) {
	defer log.Trace(item.name, "modTime=%v", modTime)("")
	item.mu.Lock()
	item._updateFingerprint()
	item._setModTime(modTime)
	item.mu.Unlock()
}

// ReadAt bytes from the file at off
func (item *Item) ReadAt(b []byte, off int64) (n int, err error) {
	item.mu.Lock()
	if item.fd == nil {
		item.mu.Unlock()
		return 0, errors.New("vfs cache item ReadAt: internal error: didn't Open file")
	}
	err = item._ensure(off, int64(len(b)))
	if err != nil {
		item.mu.Unlock()
		return n, err
	}
	item.mu.Unlock()
	return item.fd.ReadAt(b, off)
}

// WriteAt bytes to the file at off
func (item *Item) WriteAt(b []byte, off int64) (n int, err error) {
	item.mu.Lock()
	if item.fd == nil {
		item.mu.Unlock()
		return 0, errors.New("vfs cache item WriteAt: internal error: didn't Open file")
	}
	item.mu.Unlock()
	n, err = item.fd.WriteAt(b, off)
	item.mu.Lock()
	item._written(off, int64(n))
	if n > 0 {
		item.changed = true
	}
	item.mu.Unlock()
	return n, err
}

// Sync commits the current contents of the file to stable storage. Typically,
// this means flushing the file system's in-memory copy of recently written
// data to disk.
func (item *Item) Sync() error {
	if item.fd == nil {
		return errors.New("vfs cache item sync: internal error: didn't Open file")
	}
	return nil // FIXME sync the file? Or sync to backing store?
}

// rename the item
func (item *Item) rename(name string, newName string, newObj fs.Object) (err error) {
	var downloader *downloader
	// close downloader with mutex unlocked
	defer func() {
		if downloader != nil {
			_ = downloader.close(nil)
		}
	}()

	item.mu.Lock()
	defer item.mu.Unlock()

	// stop downloader
	downloader = item.downloader
	item.downloader = nil

	// Set internal state
	item.name = newName
	item.o = newObj

	// Rename cache file if it exists
	err = rename(item.c.toOSPath(name), item.c.toOSPath(newName))
	if err != nil {
		return err
	}

	// Rename meta file if it exists
	err = rename(item.c.toOSPathMeta(name), item.c.toOSPathMeta(newName))
	if err != nil {
		return err
	}

	return nil
}
