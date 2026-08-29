package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// dummyConn 是一个空实现的 net.Conn，仅用于满足 HijackedResponse.Close 调用。
type dummyConn struct{}

func (dummyConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (dummyConn) Write([]byte) (int, error)        { return 0, nil }
func (dummyConn) Close() error                     { return nil }
func (dummyConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (dummyConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (dummyConn) SetDeadline(time.Time) error      { return nil }
func (dummyConn) SetReadDeadline(time.Time) error  { return nil }
func (dummyConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "dummy" }
func (dummyAddr) String() string  { return "dummy" }

type execResult struct {
	stdout []byte
	stderr []byte
	exit   int
	cmd    []string
}

// fakeAPIClient 通过嵌入 client.APIClient 接口来满足其全部方法，
// 并覆盖测试中需要的几个方法，其余方法（如有调用）会因为嵌入字段为 nil 而 panic，
// 但我们的测试不会触达它们。
type fakeAPIClient struct {
	client.APIClient
	containers  []container.Summary
	inspect     map[string]container.InspectResponse
	imageTags   map[string][]string
	execHandler func(cmd []string) (stdout, stderr []byte, exitCode int)
	execs       map[string]*execResult
	seq         int
	listErr     error
}

func newFakeAPIClient() *fakeAPIClient {
	return &fakeAPIClient{
		inspect:   map[string]container.InspectResponse{},
		imageTags: map[string][]string{},
		execs:     map[string]*execResult{},
		execHandler: func(cmd []string) ([]byte, []byte, int) {
			return nil, nil, 0
		},
	}
}

func (f *fakeAPIClient) ContainerList(ctx context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	return client.ContainerListResult{Items: f.containers}, nil
}

func (f *fakeAPIClient) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	ins, ok := f.inspect[id]
	if !ok {
		return client.ContainerInspectResult{}, fmt.Errorf("fake: 找不到容器 %s 的 inspect 数据", id)
	}
	return client.ContainerInspectResult{Container: ins}, nil
}

func (f *fakeAPIClient) ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	tags := f.imageTags[imageID]
	if tags == nil {
		tags = []string{imageID}
	}
	r := client.ImageInspectResult{}
	r.RepoTags = tags
	return r, nil
}

func (f *fakeAPIClient) ExecCreate(ctx context.Context, id string, opts client.ExecCreateOptions) (client.ExecCreateResult, error) {
	f.seq++
	eid := fmt.Sprintf("exec-%d", f.seq)
	stdout, stderr, exit := f.execHandler(opts.Cmd)
	f.execs[eid] = &execResult{stdout: stdout, stderr: stderr, exit: exit, cmd: opts.Cmd}
	return client.ExecCreateResult{ID: eid}, nil
}

func (f *fakeAPIClient) ExecAttach(ctx context.Context, eid string, opts client.ExecAttachOptions) (client.ExecAttachResult, error) {
	res := f.execs[eid]
	var reader *bufio.Reader
	if res != nil {
		reader = stdcopyEncode(res.stdout)
	} else {
		reader = bufio.NewReader(bytes.NewReader(nil))
	}
	return client.ExecAttachResult{
		HijackedResponse: client.HijackedResponse{
			Reader: reader,
			Conn:   dummyConn{},
		},
	}, nil
}

func (f *fakeAPIClient) ExecInspect(ctx context.Context, eid string, opts client.ExecInspectOptions) (client.ExecInspectResult, error) {
	res := f.execs[eid]
	exit := 0
	if res != nil {
		exit = res.exit
	}
	return client.ExecInspectResult{ExitCode: exit}, nil
}

// stdcopyEncode 将原始数据封装为 docker stdcopy 多路复用流（stdout 帧），
// 供 HijackedResponse.Reader 被 stdcopy.StdCopy 解析使用。
func stdcopyEncode(data []byte) *bufio.Reader {
	header := make([]byte, 8)
	header[0] = 1 // STREAM_TYPE_STDOUT
	binary.BigEndian.PutUint32(header[4:8], uint32(len(data)))
	var buf bytes.Buffer
	buf.Write(header)
	buf.Write(data)
	return bufio.NewReader(&buf)
}

// dumpHandler 根据命令内容返回模拟的 exec 输出，覆盖各 provider 与 docker 方法的逻辑分支。
func dumpHandler(cmd []string) (stdout, stderr []byte, exit int) {
	c := strings.Join(cmd, " ")
	switch {
	case len(cmd) == 1 && cmd[0] == "env":
		return []byte("POSTGRES_USER=postgres\nPOSTGRES_PASSWORD=secret\nMYSQL_ROOT_PASSWORD=rootpass\n"), nil, 0
	case cmd[0] == "which":
		// which 命令：默认认为二进制不存在
		return nil, nil, 1
	case strings.Contains(c, "pg_dumpall"):
		return []byte("-- PostgreSQL database dump\n\nCREATE TABLE t (id int);\n\n-- PostgreSQL database dump complete\n"), nil, 0
	case strings.Contains(c, "pg_dump"):
		return []byte("-- PostgreSQL database dump\n\n-- PostgreSQL database dump complete\n"), nil, 0
	case strings.Contains(c, "mysqldump") || strings.Contains(c, "mariadb-dump"):
		return []byte("-- MySQL dump 10.13  Distrib 8.4.2\n\nCREATE TABLE t (id int);\n-- Dump completed on 2026-08-09 04:00:00\n"), nil, 0
	case strings.Contains(c, "redis-cli") || strings.Contains(c, "valkey-cli"):
		return []byte("REDIS0011\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), nil, 0
	case strings.Contains(c, "psql"):
		// psql -t -A -c "SELECT datname ..." 列出数据库
		return []byte("appdb\npostgres\ntemplate1\n"), nil, 0
	case strings.Contains(c, "SELECT SCHEMA_NAME"):
		return []byte("appdb\nmysql\n"), nil, 0
	}
	return nil, nil, 0
}

// newFakeDockerClient 构造一个使用 fakeAPIClient 的 dockerClient，便于单元测试。
func newFakeDockerClient(fake *fakeAPIClient) *dockerClient {
	return &dockerClient{
		api:       fake,
		envCache:  map[string]map[string]string{},
		binCache:  map[string]map[string]bool{},
		nameCache: map[string][]string{},
	}
}
