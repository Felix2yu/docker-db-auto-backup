package main

import (
	"reflect"
	"testing"
)

func testKopia() *kopiaClient {
	return &kopiaClient{cfg: &kopiaConfig{
		repositoryType:  "s3",
		repositoryFlags: "--bucket=abc --endpoint=https://localhost:9000",
		configFile:      "/tmp/kopia/repository.config",
	}}
}

func TestKopiaGlobalArgs(t *testing.T) {
	k := testKopia()
	want := []string{"--config-file", "/tmp/kopia/repository.config"}
	if got := k.globalArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("globalArgs: got %v, want %v", got, want)
	}
}

func TestKopiaRepositoryArgs(t *testing.T) {
	k := testKopia()
	want := []string{
		"repository", "connect", "s3",
		"--bucket=abc", "--endpoint=https://localhost:9000",
	}
	if got := k.repositoryArgs("connect"); !reflect.DeepEqual(got, want) {
		t.Errorf("repositoryArgs(connect): got %v, want %v", got, want)
	}
}

func TestKopiaPolicyArgs(t *testing.T) {
	k := &kopiaClient{cfg: &kopiaConfig{policyCompression: "zstd"}}
	want := []string{"policy", "set", "--global", "--compression", "zstd"}
	if got := k.policyArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("policyArgs: got %v, want %v", got, want)
	}
}
func TestKopiaPosixMappedToFilesystem(t *testing.T) {
	k := &kopiaClient{cfg: &kopiaConfig{repositoryType: "posix"}}
	want := []string{
		"repository", "create", "filesystem",
		"--path=/var/backups/kopia-repo",
	}
	k.cfg.repositoryFlags = "--path=/var/backups/kopia-repo"
	if got := k.repositoryArgs("create"); !reflect.DeepEqual(got, want) {
		t.Errorf("repositoryArgs(create): got %v, want %v", got, want)
	}
	if got := k.repositoryTypeArg(); got != "filesystem" {
		t.Errorf("repositoryTypeArg: got %q, want filesystem", got)
	}
}
