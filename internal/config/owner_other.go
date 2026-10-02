//go:build !unix

package config

func matchDirOwner(string) error { return nil }
