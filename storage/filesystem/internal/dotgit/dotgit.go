// https://github.com/git/git/blob/master/Documentation/gitrepository-layout.txt
package dotgit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	stdioutil "io/ioutil"
	"os"
	"strings"
	"time"

	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/utils/ioutil"

	"gopkg.in/src-d/go-billy.v3"
)

const (
	suffix         = ".git"
	packedRefsPath = "packed-refs"
	configPath     = "config"
	indexPath      = "index"
	shallowPath    = "shallow"
	modulePath     = "modules"
	objectsPath    = "objects"
	packPath       = "pack"
	refsPath       = "refs"

	tmpPackedRefsPrefix = "._packed-refs"

	packExt = ".pack"
	idxExt  = ".idx"
)

var (
	// ErrNotFound is returned by New when the path is not found.
	ErrNotFound = errors.New("path not found")
	// ErrIdxNotFound is returned by Idxfile when the idx file is not found
	ErrIdxNotFound = errors.New("idx file not found")
	// ErrPackfileNotFound is returned by Packfile when the packfile is not found
	ErrPackfileNotFound = errors.New("packfile not found")
	// ErrConfigNotFound is returned by Config when the config is not found
	ErrConfigNotFound = errors.New("config file not found")
	// ErrPackedRefsDuplicatedRef is returned when a duplicated reference is
	// found in the packed-ref file. This is usually the case for corrupted git
	// repositories.
	ErrPackedRefsDuplicatedRef = errors.New("duplicated ref found in packed-ref file")
	// ErrPackedRefsBadFormat is returned when the packed-ref file corrupt.
	ErrPackedRefsBadFormat = errors.New("malformed packed-ref")
	// ErrSymRefTargetNotFound is returned when a symbolic reference is
	// targeting a non-existing object. This usually means the repository
	// is corrupt.
	ErrSymRefTargetNotFound = errors.New("symbolic reference target not found")
)

// The DotGit type represents a local git repository on disk. This
// type is not zero-value-safe, use the New function to initialize it.
type DotGit struct {
	fs                billy.Filesystem
	cachedPackedRefs  refCache
	packedRefsLastMod time.Time
}

// New returns a DotGit value ready to be used. The path argument must
// be the absolute path of a git repository directory (e.g.
// "/foo/bar/.git").
func New(fs billy.Filesystem) *DotGit {
	return &DotGit{fs: fs, cachedPackedRefs: make(refCache)}
}

// Initialize creates all the folder scaffolding.
func (d *DotGit) Initialize() error {
	mustExists := []string{
		d.fs.Join("objects", "info"),
		d.fs.Join("objects", "pack"),
		d.fs.Join("refs", "heads"),
		d.fs.Join("refs", "tags"),
	}

	for _, path := range mustExists {
		_, err := d.fs.Stat(path)
		if err == nil {
			continue
		}

		if !os.IsNotExist(err) {
			return err
		}

		if err := d.fs.MkdirAll(path, os.ModeDir|os.ModePerm); err != nil {
			return err
		}
	}

	return nil
}

// ConfigWriter returns a file pointer for write to the config file
func (d *DotGit) ConfigWriter() (billy.File, error) {
	return d.fs.Create(configPath)
}

// Config returns a file pointer for read to the config file
func (d *DotGit) Config() (billy.File, error) {
	return d.fs.Open(configPath)
}

// IndexWriter returns a file pointer for write to the index file
func (d *DotGit) IndexWriter() (billy.File, error) {
	return d.fs.Create(indexPath)
}

// Index returns a file pointer for read to the index file
func (d *DotGit) Index() (billy.File, error) {
	return d.fs.Open(indexPath)
}

// ShallowWriter returns a file pointer for write to the shallow file
func (d *DotGit) ShallowWriter() (billy.File, error) {
	return d.fs.Create(shallowPath)
}

// Shallow returns a file pointer for read to the shallow file
func (d *DotGit) Shallow() (billy.File, error) {
	f, err := d.fs.Open(shallowPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	return f, nil
}

// NewObjectPack return a writer for a new packfile, it saves the packfile to
// disk and also generates and save the index for the given packfile.
func (d *DotGit) NewObjectPack(statusChan plumbing.StatusChan) (*PackWriter, error) {
	return newPackWrite(d.fs, statusChan)
}

// ObjectPacks returns the list of availables packfiles
func (d *DotGit) ObjectPacks() ([]plumbing.Hash, error) {
	packDir := d.fs.Join(objectsPath, packPath)
	files, err := d.fs.ReadDir(packDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	var packs []plumbing.Hash
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), packExt) {
			continue
		}

		n := f.Name()
		h := plumbing.NewHash(n[5 : len(n)-5]) //pack-(hash).pack
		packs = append(packs, h)

	}

	return packs, nil
}

func (d *DotGit) objectPackPath(hash plumbing.Hash, extension string) string {
	return d.fs.Join(objectsPath, packPath, fmt.Sprintf("pack-%s.%s", hash.String(), extension))
}

func (d *DotGit) objectPackOpen(hash plumbing.Hash, extension string) (billy.File, error) {
	pack, err := d.fs.Open(d.objectPackPath(hash, extension))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrPackfileNotFound
		}

		return nil, err
	}

	return pack, nil
}

// ObjectPack returns a fs.File of the given packfile
func (d *DotGit) ObjectPack(hash plumbing.Hash) (billy.File, error) {
	return d.objectPackOpen(hash, `pack`)
}

// ObjectPackIdx returns a fs.File of the index file for a given packfile
func (d *DotGit) ObjectPackIdx(hash plumbing.Hash) (billy.File, error) {
	return d.objectPackOpen(hash, `idx`)
}

func (d *DotGit) DeleteObjectPackAndIndex(hash plumbing.Hash) error {
	err := d.fs.Remove(d.objectPackPath(hash, `pack`))
	if err != nil {
		return err
	}
	err = d.fs.Remove(d.objectPackPath(hash, `idx`))
	if err != nil {
		return err
	}
	return nil
}

// NewObject return a writer for a new object file.
func (d *DotGit) NewObject() (*ObjectWriter, error) {
	return newObjectWriter(d.fs)
}

// Objects returns a slice with the hashes of objects found under the
// .git/objects/ directory.
func (d *DotGit) Objects() ([]plumbing.Hash, error) {
	var objects []plumbing.Hash
	err := d.ForEachObjectHash(func(hash plumbing.Hash) error {
		objects = append(objects, hash)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return objects, nil
}

// Objects returns a slice with the hashes of objects found under the
// .git/objects/ directory.
func (d *DotGit) ForEachObjectHash(fun func(plumbing.Hash) error) error {
	files, err := d.fs.ReadDir(objectsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	for _, f := range files {
		if f.IsDir() && len(f.Name()) == 2 && isHex(f.Name()) {
			base := f.Name()
			d, err := d.fs.ReadDir(d.fs.Join(objectsPath, base))
			if err != nil {
				return err
			}

			for _, o := range d {
				err = fun(plumbing.NewHash(base + o.Name()))
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (d *DotGit) objectPath(h plumbing.Hash) string {
	hash := h.String()
	return d.fs.Join(objectsPath, hash[0:2], hash[2:40])
}

// Object returns a fs.File pointing the object file, if exists
func (d *DotGit) Object(h plumbing.Hash) (billy.File, error) {
	return d.fs.Open(d.objectPath(h))
}

// ObjectStat returns a os.FileInfo pointing the object file, if exists
func (d *DotGit) ObjectStat(h plumbing.Hash) (os.FileInfo, error) {
	return d.fs.Stat(d.objectPath(h))
}

// ObjectDelete removes the object file, if exists
func (d *DotGit) ObjectDelete(h plumbing.Hash) error {
	return d.fs.Remove(d.objectPath(h))
}

func (d *DotGit) readReferenceFrom(rd io.Reader, name string) (ref *plumbing.Reference, err error) {
	b, err := stdioutil.ReadAll(rd)
	if err != nil {
		return nil, err
	}

	line := strings.TrimSpace(string(b))
	return plumbing.NewReferenceFromStrings(name, line), nil
}

func (d *DotGit) checkReferenceAndTruncate(f billy.File, old *plumbing.Reference) error {
	if old == nil {
		return nil
	}
	ref, err := d.readReferenceFrom(f, old.Name().String())
	if err != nil {
		return err
	}
	if ref.Hash().IsZero() {
		ref, err = d.packedRef(old.Name())
		if err != nil {
			return err
		}
	}
	if ref.Hash() != old.Hash() {
		return fmt.Errorf("reference has changed concurrently")
	}
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	err = f.Truncate(0)
	if err != nil {
		return err
	}
	return nil
}

func (d *DotGit) SetRef(r, old *plumbing.Reference) (err error) {
	var content string
	switch r.Type() {
	case plumbing.SymbolicReference:
		content = fmt.Sprintf("ref: %s\n", r.Target())
	case plumbing.HashReference:
		content = fmt.Sprintln(r.Hash().String())
	}

	// If we are not checking an old ref, just truncate the file.
	mode := os.O_RDWR | os.O_CREATE
	if old == nil {
		mode |= os.O_TRUNC
	}

	f, err := d.fs.OpenFile(r.Name().String(), mode, 0666)
	if err != nil {
		return err
	}

	defer ioutil.CheckClose(f, &err)

	// Lock is unlocked by the deferred Close above. This is because Unlock
	// does not imply a fsync and thus there would be a race between
	// Unlock+Close and other concurrent writers. Adding Sync to go-billy
	// could work, but this is better (and avoids superfluous syncs).
	err = f.Lock()
	if err != nil {
		return err
	}

	// this is a no-op to call even when old is nil.
	err = d.checkReferenceAndTruncate(f, old)
	if err != nil {
		return err
	}

	_, err = f.Write([]byte(content))
	return err
}

// Refs scans the git directory collecting references, which it returns.
// Symbolic references are resolved and included in the output.
func (d *DotGit) Refs() ([]*plumbing.Reference, error) {
	var refs []*plumbing.Reference
	var seen = make(map[plumbing.ReferenceName]bool)
	if err := d.addRefsFromRefDir(&refs, seen); err != nil {
		return nil, err
	}

	if err := d.addRefsFromPackedRefs(&refs, seen); err != nil {
		return nil, err
	}

	if err := d.addRefFromHEAD(&refs); err != nil {
		return nil, err
	}

	return refs, nil
}

// Ref returns the reference for a given reference name.
func (d *DotGit) Ref(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	ref, err := d.readReferenceFile(".", name.String())
	if err == nil {
		return ref, nil
	}

	return d.packedRef(name)
}

func (d *DotGit) syncPackedRefs() (err error) {
	fi, err := d.fs.Stat(packedRefsPath)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}

	if d.packedRefsLastMod.Before(fi.ModTime()) {
		d.cachedPackedRefs = make(refCache)
		f, err := d.fs.Open(packedRefsPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		defer ioutil.CheckClose(f, &err)

		s := bufio.NewScanner(f)
		for s.Scan() {
			ref, err := d.processLine(s.Text())
			if err != nil {
				return err
			}

			if ref != nil {
				d.cachedPackedRefs[ref.Name()] = ref
			}
		}

		d.packedRefsLastMod = fi.ModTime()

		return s.Err()
	}

	return nil
}

func (d *DotGit) packedRef(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if err := d.syncPackedRefs(); err != nil {
		return nil, err
	}

	if ref, ok := d.cachedPackedRefs[name]; ok {
		return ref, nil
	}

	return nil, plumbing.ErrReferenceNotFound
}

// RemoveRef removes a reference by name.
func (d *DotGit) RemoveRef(name plumbing.ReferenceName) error {
	path := d.fs.Join(".", name.String())
	_, err := d.fs.Stat(path)
	if err == nil {
		err = d.fs.Remove(path)
		// Drop down to remove it from the packed refs file, too.
	}

	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return d.rewritePackedRefsWithoutRef(name)
}

func (d *DotGit) addRefsFromPackedRefs(refs *[]*plumbing.Reference, seen map[plumbing.ReferenceName]bool) (err error) {
	if err := d.syncPackedRefs(); err != nil {
		return err
	}

	for name, ref := range d.cachedPackedRefs {
		if !seen[name] {
			*refs = append(*refs, ref)
			seen[name] = true
		}
	}

	return nil
}

func (d *DotGit) rewritePackedRefsWithoutRef(name plumbing.ReferenceName) (err error) {
	f, err := d.fs.Open(packedRefsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}
	defer ioutil.CheckClose(f, &err)

	err = f.Lock()
	if err != nil {
		return err
	}

	// Re-open the file after locking, since it could have been
	// renamed over by a new file during the Lock process.
	pr, err := d.fs.Open(packedRefsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}
	doClosePR := true
	defer func() {
		if doClosePR {
			ioutil.CheckClose(pr, &err)
		}
	}()

	// Creating the temp file in the same directory as the target file
	// improves our chances for rename operation to be atomic.
	tmp, err := d.fs.TempFile("", tmpPackedRefsPrefix)
	if err != nil {
		return err
	}
	doCloseTmp := true
	defer func() {
		if doCloseTmp {
			ioutil.CheckClose(tmp, &err)
		}
	}()

	s := bufio.NewScanner(pr)
	found := false
	for s.Scan() {
		line := s.Text()
		ref, err := d.processLine(line)
		if err != nil {
			return err
		}

		if ref != nil && ref.Name() == name {
			found = true
			continue
		}

		if _, err := fmt.Fprintln(tmp, line); err != nil {
			return err
		}
	}

	if err := s.Err(); err != nil {
		return err
	}

	if !found {
		doCloseTmp = false
		ioutil.CheckClose(tmp, &err)
		if err != nil {
			return err
		}
		// Delete the temp file if nothing needed to be removed.
		return d.fs.Remove(tmp.Name())
	}

	doClosePR = false
	if err := pr.Close(); err != nil {
		return err
	}

	doCloseTmp = false
	if err := tmp.Close(); err != nil {
		return err
	}

	return d.fs.Rename(tmp.Name(), packedRefsPath)
}

// process lines from a packed-refs file
func (d *DotGit) processLine(line string) (*plumbing.Reference, error) {
	if len(line) == 0 {
		return nil, nil
	}

	switch line[0] {
	case '#': // comment - ignore
		return nil, nil
	case '^': // annotated tag commit of the previous line - ignore
		return nil, nil
	default:
		ws := strings.Split(line, " ") // hash then ref
		if len(ws) != 2 {
			return nil, ErrPackedRefsBadFormat
		}

		return plumbing.NewReferenceFromStrings(ws[1], ws[0]), nil
	}
}

func (d *DotGit) addRefsFromRefDir(refs *[]*plumbing.Reference, seen map[plumbing.ReferenceName]bool) error {
	return d.walkReferencesTree(refs, []string{refsPath}, seen)
}

func (d *DotGit) walkReferencesTree(refs *[]*plumbing.Reference, relPath []string, seen map[plumbing.ReferenceName]bool) error {
	files, err := d.fs.ReadDir(d.fs.Join(relPath...))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	for _, f := range files {
		newRelPath := append(append([]string(nil), relPath...), f.Name())
		if f.IsDir() {
			if err = d.walkReferencesTree(refs, newRelPath, seen); err != nil {
				return err
			}

			continue
		}

		ref, err := d.readReferenceFile(".", strings.Join(newRelPath, "/"))
		if err != nil {
			return err
		}

		if ref != nil && !seen[ref.Name()] {
			*refs = append(*refs, ref)
			seen[ref.Name()] = true
		}
	}

	return nil
}

func (d *DotGit) addRefFromHEAD(refs *[]*plumbing.Reference) error {
	ref, err := d.readReferenceFile(".", "HEAD")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	*refs = append(*refs, ref)
	return nil
}

func (d *DotGit) readReferenceFile(path, name string) (ref *plumbing.Reference, err error) {
	path = d.fs.Join(path, d.fs.Join(strings.Split(name, "/")...))
	f, err := d.fs.Open(path)
	if err != nil {
		return nil, err
	}
	defer ioutil.CheckClose(f, &err)

	return d.readReferenceFrom(f, name)
}

func (d *DotGit) SetPackedRefs(refs []plumbing.Reference) (err error) {
	// Lock it using a temp file.  TODO: clean this up?
	lockFile, err := d.fs.Create(tmpPackedRefsPrefix)
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(lockFile, &err)

	err = lockFile.Lock()
	if err != nil {
		return err
	}

	f, err := d.fs.Create(packedRefsPath)
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(f, &err)

	// Check that the file is empty. Technically the locked create
	// above should fail if the file exists yet, but let's just be
	// safe and check.
	buf, err := stdioutil.ReadAll(f)
	if err != nil {
		return err
	}
	if len(buf) != 0 {
		return errors.New("packed-refs file already initialized")
	}

	w := bufio.NewWriter(f)
	for _, ref := range refs {
		_, err := w.WriteString(ref.String() + "\n")
		if err != nil {
			return err
		}
	}
	return w.Flush()
}

func (d *DotGit) CountLooseRefs() (int, error) {
	var refs []*plumbing.Reference
	var seen = make(map[plumbing.ReferenceName]bool)
	if err := d.addRefsFromRefDir(&refs, seen); err != nil {
		return 0, err
	}

	return len(refs), nil
}

// PackRefs packs all loose refs into the packed-refs file.
//
// This implementation only works under the assumption that the view
// of the file system won't be updated during this operation, which is
// true for kbfsgit after the Lock() operation is complete (and before
// the Unlock()/Close() of the locked file).  If another process
// concurrently updates one of the loose refs we delete, then KBFS
// conflict resolution would just end up ignoring our delete.  Also
// note that deleting a ref requires locking packed-refs, so a ref
// deleted by the user shouldn't be revived by ref-packing.
//
// The strategy would not work on a general file system though,
// without locking each loose reference and checking it again before
// deleting the file, because otherwise an updated reference could
// sneak in and then be deleted by the packed-refs process.
// Alternatively, every ref update could also lock packed-refs, so
// only one lock is required during ref-packing.  But that would
// worsen performance in the common case.
//
// TODO: before trying to get this merged upstream, move it into a
// custom kbfsgit Storer implementation, and rewrite this function to
// work correctly on a general filesystem.
//
// TODO: add an "all" boolean like the `git pack-refs --all` flag.
// When `all` is false, it would only pack refs that have already been
// packed, plus all tags.
func (d *DotGit) PackRefs() (err error) {
	// Lock packed-refs, and create it if it doesn't exist yet.
	f, err := d.fs.OpenFile(packedRefsPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(f, &err)

	err = f.Lock()
	if err != nil {
		return err
	}

	// Gather all refs using addRefsFromRefDir and addRefsFromPackedRefs.
	var refs []*plumbing.Reference
	var seen = make(map[plumbing.ReferenceName]bool)
	if err := d.addRefsFromRefDir(&refs, seen); err != nil {
		return err
	}
	if len(refs) == 0 {
		// Nothing to do!
		return nil
	}
	numLooseRefs := len(refs)
	if err := d.addRefsFromPackedRefs(&refs, seen); err != nil {
		return err
	}

	// Write them all to a new temp packed-refs file.
	tmp, err := d.fs.TempFile("", tmpPackedRefsPrefix)
	if err != nil {
		return err
	}
	doCloseTmp := true
	defer func() {
		if doCloseTmp {
			ioutil.CheckClose(tmp, &err)
		}
	}()
	w := bufio.NewWriter(tmp)
	for _, ref := range refs {
		_, err := w.WriteString(ref.String() + "\n")
		if err != nil {
			return err
		}
	}
	err = w.Flush()
	if err != nil {
		return err
	}

	// Rename the temp packed-refs file.
	doCloseTmp = false
	if err := tmp.Close(); err != nil {
		return err
	}
	err = d.fs.Rename(tmp.Name(), packedRefsPath)
	if err != nil {
		return err
	}

	// Delete all the loose refs, while still holding the packed-refs
	// lock.
	for _, ref := range refs[:numLooseRefs] {
		path := d.fs.Join(".", ref.Name().String())
		err = d.fs.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	// Update packed-refs cache.
	d.cachedPackedRefs = make(refCache)
	for _, ref := range refs {
		d.cachedPackedRefs[ref.Name()] = ref
	}
	d.packedRefsLastMod = time.Now()

	return nil
}

// Module return a billy.Filesystem poiting to the module folder
func (d *DotGit) Module(name string) (billy.Filesystem, error) {
	return d.fs.Chroot(d.fs.Join(modulePath, name))
}

func isHex(s string) bool {
	for _, b := range []byte(s) {
		if isNum(b) {
			continue
		}
		if isHexAlpha(b) {
			continue
		}

		return false
	}

	return true
}

func isNum(b byte) bool {
	return b >= '0' && b <= '9'
}

func isHexAlpha(b byte) bool {
	return b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

type refCache map[plumbing.ReferenceName]*plumbing.Reference
