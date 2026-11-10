// package main implements a simple in-memory filesystem
// Pre-requisites: libfuse3-dev, gcc, add build tags to settings.json
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/winfsp/cgofuse/fuse"
)

func split(path string) []string {
	return strings.Split(path, "/")
}

type node struct {
	stat      fuse.Stat_t
	child     map[string]*node
	attr      map[string][]byte
	data      []byte
	openCount int
}

func newNode(deviceID uint64, inode uint64, mode uint32, userID uint32, groupID uint32) *node {
	tmstmp := fuse.Now()

	n := node{
		fuse.Stat_t{
			Dev:      deviceID,
			Ino:      inode,
			Mode:     mode,
			Uid:      userID,
			Gid:      groupID,
			Atim:     tmstmp,
			Mtim:     tmstmp,
			Ctim:     tmstmp,
			Birthtim: tmstmp,
		},
		nil, nil, nil, 0,
	}

	// extract the file type bits from the rest of the inode mode bits
	// and check if the inode is directory
	if n.stat.Mode&fuse.S_IFMT == fuse.S_IFDIR {
		n.child = map[string]*node{}
	}

	return &n
}

type FileSystem struct {
	fuse.FileSystemBase
	mtx          sync.Mutex
	inode        uint64
	root         *node
	openInodeMap map[uint64]*node
}

func NewFileSystem() *FileSystem {
	fs := FileSystem{}

	defer fs.sync()()

	fs.inode++
	fs.root = newNode(0, fs.inode, fuse.S_IFDIR|00774, 0, 0)
	fs.openInodeMap = map[uint64]*node{}

	return &fs
}

func (fs *FileSystem) sync() func() {
	fs.mtx.Lock()

	return func() {
		fs.mtx.Unlock()
	}
}

func (fs *FileSystem) lookupNode(path string, ancestor *node) (parent *node, name string, node *node) {
	parent = fs.root
	name = ""
	node = fs.root

	for _, p := range split(path) {
		if p != "" {
			if 255 < utf8.RuneCountInString(p) {
				panic(fuse.Error(-fuse.ENAMETOOLONG))
			}

			parent, name = node, p

			if node == nil {
				return
			}

			node = node.child[p]

			if ancestor != nil && node == ancestor {
				name = ""
				return
			}
		}
	}

	return
}

func (fs *FileSystem) makeNode(path string, mode uint32, deviceID uint64, data []byte) int {
	parent, name, node := fs.lookupNode(path, nil)

	if parent == nil {
		return -fuse.ENOENT
	}

	if node != nil {
		return -fuse.EEXIST
	}

	fs.inode++
	uid, gid, _ := fuse.Getcontext()
	node = newNode(deviceID, fs.inode, mode, uid, gid)
	if data != nil {
		node.data = make([]byte, len(data))
		node.stat.Size = int64(len(data))
		copy(node.data, data)
	}

	parent.child[name] = node
	parent.stat.Ctim = node.stat.Ctim
	parent.stat.Mtim = node.stat.Mtim

	return 0
}

func (fs *FileSystem) deleteNode(path string, dir bool) int {
	parent, name, node := fs.lookupNode(path, nil)

	if node == nil {
		return -fuse.ENOENT
	}

	if !dir && fuse.S_IFDIR == fuse.S_IFMT&node.stat.Mode {
		return -fuse.EISDIR
	}

	if dir && fuse.S_IFDIR != fuse.S_IFMT&node.stat.Mode {
		return -fuse.ENOTDIR
	}

	if len(node.child) > 0 {
		return -fuse.ENOTEMPTY
	}

	node.stat.Nlink--
	delete(parent.child, name)
	tmstmp := fuse.Now()
	node.stat.Atim = tmstmp
	parent.stat.Ctim = tmstmp
	parent.stat.Mtim = tmstmp

	return 0
}

func (fs *FileSystem) openNode(path string, dir bool) (int, uint64) {
	_, _, node := fs.lookupNode(path, nil)

	if node == nil {
		return -fuse.ENOENT, ^uint64(0)
	}

	if !dir && fuse.S_IFDIR == fuse.S_IFMT&node.stat.Mode {
		return -fuse.EISDIR, ^uint64(0)
	}

	if dir && fuse.S_IFDIR != fuse.S_IFMT&node.stat.Mode {
		return -fuse.ENOTDIR, ^uint64(0)
	}

	node.openCount++

	if node.openCount == 1 {
		fs.openInodeMap[node.stat.Ino] = node
	}

	return 0, node.stat.Ino
}

func (fs *FileSystem) closeNode(fileHandler uint64) int {
	node := fs.openInodeMap[fileHandler]
	node.openCount--

	if node.openCount == 0 {
		delete(fs.openInodeMap, node.stat.Ino)
	}

	return 0
}

func (fs *FileSystem) getNode(path string, fileHandler uint64) *node {
	if ^uint64(0) == fileHandler {
		_, _, node := fs.lookupNode(path, nil)

		return node
	} else {
		return fs.openInodeMap[fileHandler]
	}
}

// Mkdir creates a new directory.
func (fs *FileSystem) Mkdir(path string, mode uint32) int {
	defer fs.sync()()

	return fs.makeNode(path, fuse.S_IFDIR|(mode&07777), 0, nil)
}

// Rmdir deletes an existing directory.
func (fs *FileSystem) Rmdir(path string) int {
	defer fs.sync()()

	return fs.deleteNode(path, true)
}

// Rename renames a file/directory.
func (fs *FileSystem) Rename(oldpath, newpath string) int {
	defer fs.sync()()

	oldParent, oldName, oldNode := fs.lookupNode(oldpath, nil)

	if oldNode == nil {
		return -fuse.ENOENT
	}

	newParent, newName, newNode := fs.lookupNode(newpath, nil)

	if newParent == nil {
		return -fuse.ENOENT
	}

	if newName == "" {
		return -fuse.EINVAL
	}

	if oldParent == newParent && oldName == newName {
		return 0
	}

	if newNode != nil {
		err := fs.deleteNode(newpath, fuse.S_IFDIR == oldNode.stat.Mode&fuse.S_IFMT)
		if err != 0 {
			return err
		}
	}

	delete(oldParent.child, oldName)
	newParent.child[newName] = oldNode

	return 0
}

// Chmod changes the file mode.
func (fs *FileSystem) Chmod(path string, mode uint32) int {
	defer fs.sync()()

	_, _, node := fs.lookupNode(path, nil)

	if node == nil {
		return -fuse.ENOENT
	}

	node.stat.Mode = (node.stat.Mode & fuse.S_IFMT) | mode&07777
	node.stat.Ctim = fuse.Now()

	return 0
}

// Chown changes the file ownership.
func (fs *FileSystem) Chown(path string, uid, gid uint32) int {
	defer fs.sync()()

	_, _, node := fs.lookupNode(path, nil)

	if node == nil {
		return -fuse.ENOENT
	}

	if uid != ^uint32(0) {
		node.stat.Uid = uid
	}

	if gid != ^uint32(0) {
		node.stat.Gid = gid
	}

	node.stat.Ctim = fuse.Now()

	return 0
}

// Open opens a file.
func (fs *FileSystem) Open(path string, _ int) (int, uint64) {
	defer fs.sync()()

	return fs.openNode(path, false)
}

// Opendir opens a directory.
func (fs *FileSystem) Opendir(path string) (int, uint64) {
	defer fs.sync()()

	return fs.openNode(path, true)
}

// Release frees the resources held by a node.
func (fs *FileSystem) Release(_ string, fileHandler uint64) int {
	defer fs.sync()()

	return fs.closeNode(fileHandler)
}

// Read reads the contents of a node.
func (fs *FileSystem) Read(path string, buf []byte, offset int64, fileHandler uint64) int {
	defer fs.sync()()

	node := fs.getNode(path, fileHandler)

	if node == nil {
		return -fuse.ENOENT
	}

	endOffset := offset + int64(len(buf))
	endOffset = min(endOffset, node.stat.Size)

	if endOffset < offset {
		return 0
	}

	n := copy(buf, node.data[offset:endOffset])
	node.stat.Atim = fuse.Now()

	return n
}

// Getattr gets the file attributes.
func (fs *FileSystem) Getattr(path string, stat *fuse.Stat_t, fileHandler uint64) int {
	defer fs.sync()()

	node := fs.getNode(path, fileHandler)

	if node == nil {
		return -fuse.ENOENT
	}

	*stat = node.stat

	return 0
}

// Readdir reads a directory.
func (fs *FileSystem) Readdir(
	path string,
	fill func(string, *fuse.Stat_t, int64) bool,
	offset int64,
	fileHandler uint64,
) int {
	defer fs.sync()()

	node := fs.openInodeMap[fileHandler]
	fill(".", &node.stat, 0)
	fill("..", nil, 0)

	for name, child := range node.child {
		if !fill(name, &child.stat, 0) {
			break
		}
	}

	return 0
}

// Releasedir closes an open directory.
func (fs *FileSystem) Releasedir(_ string, fileHandler uint64) int {
	defer fs.sync()()

	return fs.closeNode(fileHandler)
}

// Utimens changes the file access timestamps.
func (fs *FileSystem) Utimens(path string, timestamp []fuse.Timespec) int {
	defer fs.sync()()

	_, _, node := fs.lookupNode(path, nil)

	if node == nil {
		return -fuse.ENOENT
	}

	node.stat.Ctim = fuse.Now()

	if timestamp == nil {
		tm := node.stat.Ctim
		tmsp := [2]fuse.Timespec{tm, tm}
		timestamp = tmsp[:]
	}

	node.stat.Atim = timestamp[0]
	node.stat.Mtim = timestamp[1]

	return 0
}

// Unlink removes a file.
func (fs *FileSystem) Unlink(path string) int {
	defer fs.sync()()

	return fs.deleteNode(path, false)
}

func resize(slice []byte, size int64, zeroinit bool) []byte {
	const allocUnit = 64 * 1024
	allocSize := (size + allocUnit - 1) / allocUnit * allocUnit

	if cap(slice) != int(allocSize) {
		var newSlice []byte
		{
			defer func() {
				if r := recover(); r != nil {
					panic(fuse.Error(-fuse.ENOSPC))
				}
			}()

			newSlice = make([]byte, size, allocSize)
		}

		copy(newSlice, slice)
		slice = newSlice
	} else if zeroinit {
		i := len(slice)
		slice := slice[:size]
		for ; len(slice) > i; i++ {
			slice[i] = 0
		}
	}

	return slice
}

// Write writes data to a file.
func (fs *FileSystem) Write(path string, buf []byte, offset int64, fileHandler uint64) int {
	defer fs.sync()()

	node := fs.getNode(path, fileHandler)
	if node == nil {
		return -fuse.ENOENT
	}

	endOffset := offset + int64(len(buf))
	if endOffset > node.stat.Size {
		node.data = resize(node.data, endOffset, true)
		node.stat.Size = endOffset
	}

	n := copy(node.data[offset:endOffset], buf)
	tmsp := fuse.Now()
	node.stat.Ctim = tmsp
	node.stat.Mtim = tmsp

	return n
}

// Truncate changes the size of a file.
func (fs *FileSystem) Truncate(path string, size int64, fileHandler uint64) int {
	defer fs.sync()()

	node := fs.getNode(path, fileHandler)
	if node == nil {
		return -fuse.ENOENT
	}

	node.data = resize(node.data, size, true)
	node.stat.Size = size
	tmsp := fuse.Now()
	node.stat.Ctim = tmsp
	node.stat.Mtim = tmsp

	return 0
}

// Mknod creates a file node.
func (fs *FileSystem) Mknod(path string, mode uint32, deviceID uint64) int {
	defer fs.sync()()

	return fs.makeNode(path, mode, deviceID, nil)
}

func main() {
	fs := NewFileSystem()
	host := fuse.NewFileSystemHost(fs)
	host.SetCapReaddirPlus(true)
	host.SetUseIno(true)
	mountpoint := os.Args[1]
	opts := os.Args[2:]
	host.Mount(mountpoint, opts)

	fmt.Println("closing the test fs")
}
