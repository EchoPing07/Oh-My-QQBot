package logx

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRotateSameSecondKeepsAllBytes 回归 [P1-1]：同一秒内多次轮转不许互相覆盖。
//
// 旧实现的归档名只有秒级精度，而 os.Rename（Windows 的
// MOVEFILE_REPLACE_EXISTING / POSIX rename(2)）是覆盖语义 —— 同秒内后一次
// 轮转把前一份归档吃掉。实测写 4.3MB 只剩 716KB，且没有任何错误。
func TestRotateSameSecondKeepsAllBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oh-my-qqbot.log")
	rf, err := newRotatingFile(path, 1, 50, nil)
	if err != nil {
		t.Fatalf("创建文件 writer 失败: %v", err)
	}

	chunk := bytes.Repeat([]byte("x"), 700*1024)
	const rounds = 6
	total := 0
	for i := 0; i < rounds; i++ {
		n, err := rf.Write(chunk)
		if err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i, err)
		}
		total += n
	}

	// 统计前必须 Close：NTFS 对「打开中的句柄」的目录枚举大小是惰性更新的
	// （实测写入 716800 字节后 ReadDir 仍显示 0），句柄关闭后元数据才刷新。
	// 不关的话当前文件会被统计成 0 字节，断言恒差一个 chunk。
	if err := rf.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 断言「所有归档文件字节数总和 + 当前文件 == 写入总量」。
	var got int64
	archives := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读日志目录失败: %v", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s 失败: %v", e.Name(), err)
		}
		got += info.Size()
		if e.Name() != filepath.Base(path) {
			archives++
		}
	}
	if got != int64(total) {
		t.Errorf("落盘 %d 字节，期望 %d（同秒轮转覆盖丢了 %d 字节）", got, total, int64(total)-got)
	}
	if archives < rounds-2 {
		t.Errorf("归档数 = %d，期望至少 %d（共写入 %d 轮）", archives, rounds-2, rounds)
	}
}

// TestRotateRenameFailureFallsBackToAppend 回归 [P1-1]/[P2-2]：
// 归档改名失败不许静默，回退为「重新打开原文件继续追加」。
//
// rename 必失败的场景按平台构造：
//   - Windows：外部只读句柄占住日志文件。Go 打开文件的 share mode
//     不含 FILE_SHARE_DELETE，MoveFileEx 移动它时 sharing violation
//     （自己删自己是删不掉的——rf 还开着句柄，Windows 不允许删除
//     打开中的文件，所以「删掉源文件」这条路在本机走不通）。
//   - POSIX：rename(2) 不受打开句柄影响，直接删掉源文件让 rename 报 ENOENT。
//
// 期望：告警恰好一次、后续写入仍落到原路径、恢复后正常轮转不再告警、
// 再次失效时（degraded 已被恢复清除）再告警一次。
func TestRotateRenameFailureFallsBackToAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oh-my-qqbot.log")
	var warns int32
	rf, err := newRotatingFile(path, 1, 5, func(err error) {
		if atomic.AddInt32(&warns, 1) == 1 && !strings.Contains(err.Error(), "日志轮转失败") {
			t.Errorf("首次告警内容不像轮转失败: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("创建文件 writer 失败: %v", err)
	}
	defer func() { _ = rf.Close() }()

	// blockRename 构造「下一次 rotate 的 os.Rename 必失败」，返回解除函数。
	blockRename := func() (unblock func()) {
		if runtime.GOOS == "windows" {
			f, err := os.Open(path) // 只读句柄即可：rename 缺 FILE_SHARE_DELETE 就失败
			if err != nil {
				t.Fatalf("占用日志文件失败: %v", err)
			}
			return func() { _ = f.Close() }
		}
		if err := os.Remove(path); err != nil {
			t.Fatalf("删除日志文件失败: %v", err)
		}
		return func() {}
	}

	chunk := bytes.Repeat([]byte("a"), 700*1024)
	if _, err := rf.Write(chunk); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}

	// 第一轮：rename 失败 → 告警 1 次 → 回退追加，写入不丢。
	unblock := blockRename()
	if _, err := rf.Write(chunk); err != nil {
		t.Fatalf("轮转失败后的写入应回退为追加而不是报错: %v", err)
	}
	if atomic.LoadInt32(&warns) != 1 {
		t.Errorf("告警次数 = %d，期望恰好 1 次（避免风暴）", warns)
	}
	// 回退追加的字节必须完整落盘。两种平台的文件大小不同
	// （Windows 是在原文件上继续追加、POSIX 是重建后写入），
	// 但写入内容都必须出现在文件尾部。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回日志文件失败: %v", err)
	}
	if len(raw) < len(chunk) || !bytes.Equal(raw[len(raw)-len(chunk):], chunk) {
		t.Errorf("回退追加后文件尾部不是本次写入（文件 %d 字节）", len(raw))
	}

	// 恢复后的下一次轮转应正常归档（rename 源存在且未被占用），不再告警。
	unblock()
	if _, err := rf.Write(chunk); err != nil {
		t.Fatalf("恢复后的写入失败: %v", err)
	}
	if atomic.LoadInt32(&warns) != 1 {
		t.Errorf("恢复后的正常轮转不该再告警: %d", warns)
	}

	// 再失效一次：恢复时已清 degraded 标志，这次要再告警一次。
	unblock = blockRename()
	defer unblock()
	if _, err := rf.Write(chunk); err != nil {
		t.Fatalf("第二次失效的写入失败: %v", err)
	}
	if atomic.LoadInt32(&warns) != 2 {
		t.Errorf("恢复后的下一次失效应再次告警（共 2 次），实际 %d", warns)
	}
}

// TestWriteOpenFailureWarnsOnce 回归 [P2-2]：文件打不开时不许静默。
//
// slog 会忽略 Handle 返回的错误，所以不主动告警的话这条路径完全无痕
// （磁盘满 / 目录被换成同名文件时日志彻底静默，问题现场全丢）。
func TestWriteOpenFailureWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oh-my-qqbot.log")
	var warns int32
	rf, err := newRotatingFile(path, 1, 5, func(error) { atomic.AddInt32(&warns, 1) })
	if err != nil {
		t.Fatalf("创建文件 writer 失败: %v", err)
	}
	defer func() { _ = rf.Close() }()

	// 人为把文件出口置为「未打开」并把路径换成目录（OpenFile 必失败）。
	rf.mu.Lock()
	_ = rf.f.Close()
	rf.f = nil
	rf.mu.Unlock()
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除日志文件失败: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("占位目录创建失败: %v", err)
	}

	if _, err := rf.Write([]byte("first")); err == nil {
		t.Error("open 失败时 Write 应返回错误")
	}
	if _, err := rf.Write([]byte("second")); err == nil {
		t.Error("open 持续失败时 Write 应继续返回错误")
	}
	if atomic.LoadInt32(&warns) != 1 {
		t.Errorf("告警次数 = %d，期望恰好 1 次", warns)
	}

	// 恢复：去掉占位目录后 open 成功，告警不再增加。
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("移除占位目录失败: %v", err)
	}
	if _, err := rf.Write([]byte("recovered")); err != nil {
		t.Fatalf("恢复后的写入失败: %v", err)
	}
	if atomic.LoadInt32(&warns) != 1 {
		t.Errorf("恢复后不该再告警: %d", warns)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "recovered") {
		t.Errorf("恢复后的写入没落盘: %v %q", err, raw)
	}
}

// TestCloseDiscardsSubsequentWrites 回归 [P2-4]：Close 之后不许重新打开文件。
//
// 旧实现里 handler 仍持有 rotatingFile，下一次 Write 检测到 f==nil 就 open，
// 于是「Close → Info(已关闭)」会把日志文件重新建出来。
func TestCloseDiscardsSubsequentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oh-my-qqbot.log")
	rf, err := newRotatingFile(path, 1, 3, nil)
	if err != nil {
		t.Fatalf("创建文件 writer 失败: %v", err)
	}
	if _, err := rf.Write([]byte("first\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := rf.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除日志文件失败: %v", err)
	}

	msg := []byte("after-close\n")
	n, err := rf.Write(msg)
	if err != nil {
		t.Fatalf("Close 后的写入应被丢弃而不是报错: %v", err)
	}
	if n != len(msg) {
		t.Errorf("丢弃式写入返回 %d，期望 %d", n, len(msg))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Close 后日志文件被重新创建了: %v", err)
	}
}
