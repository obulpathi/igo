// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package token

type serializedFile struct /* fields correspond 1:1 to fields with same (lower-case) name in File  */ 
{
	Name  string
	Base  int
	Size  int
	Lines []int
	Infos []lineInfo
}
type serializedFileSet struct {
	Base  int
	Files []serializedFile
}

// Read calls decode to deserialize a file set into s; s must not be nil.
func (self *FileSet) Read(decode func(interface{}) error) error {
	var ss serializedFileSet
	if err := decode(&ss); err != nil {
		return err

	}
	self.mutex.Lock()
	self.base = ss.Base
	files := make([]*File, len(ss.Files))
	for i := 0; i < len(ss.Files); i++ {
		f := &ss.Files[i]
		files[i] = &File{self, f.Name, f.Base, f.Size, f.Lines, f.Infos}

	}
	self.files = files
	self.last = nil
	self.mutex.Unlock()

	return nil
}

// Write calls encode to serialize the file set s.
func (self *FileSet) Write(encode func(interface{}) error) error {
	var ss serializedFileSet

	self.mutex.Lock()
	ss.Base = self.base
	files := make([]serializedFile, len(self.files))
	for i, f := range self.files {
		files[i] = serializedFile{f.name, f.base, f.size, f.lines, f.infos}

	}
	ss.Files = files
	self.mutex.Unlock()

	return encode(ss)
}
