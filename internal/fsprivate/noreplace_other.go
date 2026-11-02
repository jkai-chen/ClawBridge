//go:build !windows && !linux && !darwin

package fsprivate

func renameNoReplace(oldpath, newpath string) error { return linkNoReplace(oldpath, newpath) }
