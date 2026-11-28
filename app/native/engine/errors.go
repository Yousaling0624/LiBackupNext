package engine

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/restic/restic/internal/repository"
)

// Reason 是跨进程的稳定失败原因标识，和 Kotlin 的 FailureReason.wire 逐字对应。
type Reason string

const (
	ReasonPassword    Reason = "password"
	ReasonUnreachable Reason = "unreachable"
	ReasonAuth        Reason = "auth"
	ReasonTimeout     Reason = "timeout"
	ReasonNoSpace     Reason = "no_space"
	ReasonCorruptData Reason = "corrupt_data"
)

func (r Reason) isTransport() bool {
	return r == ReasonUnreachable || r == ReasonAuth || r == ReasonTimeout
}

// Failure 带一段底层原文和一个可选原因。Message 不是给用户看的文案：
// 已知原因时主进程只显示一句中文，原文只进日志；未知原因才拿它兜底。
type Failure struct {
	Message string
	Reason  Reason
}

func (f *Failure) Error() string { return f.Message }

func (f *Failure) ReasonWire() string {
	if f.Reason == "" {
		return ""
	}
	return string(f.Reason)
}

func appFailure(message string) *Failure {
	return &Failure{Message: message}
}

func appFailuref(format string, args ...any) *Failure {
	return &Failure{Message: fmt.Sprintf(format, args...)}
}

func wrapFailure(prefix string, err error) *Failure {
	f := DescribeError(err)
	f.Message = prefix + f.Message
	return f
}

// errCancelled 是传输层失败后短路后续读写用的错误。文本带 rest 请求特征，
// 即使中断帧没赶上、最终响应先返回，主进程仍会按断连处理。
var errCancelled = errors.New("error sending request for url (cancelled)")

type idleTimeoutError struct{}

func (idleTimeoutError) Error() string   { return "i/o timeout: remote did not respond for too long" }
func (idleTimeoutError) Timeout() bool   { return true }
func (idleTimeoutError) Temporary() bool { return true }

// classifyError 是失败原因的唯一判定点。认不出来返回空，让主进程显示原文。
func classifyError(err error) Reason {
	if err == nil {
		return ""
	}

	if errors.Is(err, repository.ErrNoKeyFound) {
		return ReasonPassword
	}

	text := err.Error()

	// HTTP 状态先判：rest 后端把 401/403 作为权限问题，其他状态都算通道不通。
	if strings.Contains(text, "unexpected HTTP response (401)") ||
		strings.Contains(text, "unexpected HTTP response (403)") ||
		strings.Contains(text, "401 Unauthorized") ||
		strings.Contains(text, "403 Forbidden") {
		return ReasonAuth
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ReasonTimeout
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return ReasonTimeout
	}

	if errors.Is(err, syscall.ENOSPC) || strings.Contains(text, "No space left on device") {
		return ReasonNoSpace
	}

	if errors.Is(err, os.ErrNotExist) {
		return ReasonCorruptData
	}

	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "wrong password or no key found"):
		return ReasonPassword
	case strings.Contains(lower, "i/o timeout"),
		strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "timed out"):
		return ReasonTimeout
	case strings.Contains(lower, "ciphertext verification failed"),
		strings.Contains(lower, "invalid data"),
		strings.Contains(lower, "hash mismatch"),
		strings.Contains(lower, "does not exist"),
		strings.Contains(lower, "not found in repository"),
		strings.Contains(lower, "repository is damaged"),
		strings.Contains(lower, "damaged: "),
		strings.Contains(lower, "unexpected eof"):
		return ReasonCorruptData
	case strings.Contains(lower, "error sending request for url"),
		strings.Contains(lower, "status server error"),
		strings.Contains(lower, "status client error"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connection reset by peer"),
		strings.Contains(lower, "no route to host"),
		strings.Contains(lower, "network is unreachable"),
		strings.Contains(lower, "dial tcp"),
		strings.Contains(lower, "server misbehaving"),
		strings.Contains(lower, "unexpected http response"),
		strings.Contains(lower, "server gave http response to https client"),
		strings.Contains(lower, "tls:"),
		strings.Contains(lower, "x509:"),
		strings.Contains(lower, "broken pipe"),
		strings.Contains(lower, "stream error"):
		return ReasonUnreachable
	}

	return ""
}

// DescribeError 去掉错误文本里的文档链接 / issue 模板 / Backtrace，并抹掉 URL 里的
// userinfo（rest_url 的随机 token 只该留在内存里，日志和 UI 都不能出现）。
func DescribeError(err error) *Failure {
	if err == nil {
		return appFailure("未知错误")
	}

	text := err.Error()
	cleaned := cleanErrorText(text)
	if cleaned == "" {
		cleaned = text
	}

	return &Failure{
		Message: redactUserinfo(cleaned),
		Reason:  classifyError(err),
	}
}

func cleanErrorText(text string) string {
	var out strings.Builder
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n\n") {
				out.WriteByte('\n')
			}
			continue
		}
		if strings.HasPrefix(line, "Backtrace:") {
			break
		}
		if strings.HasPrefix(line, "For more information, see:") ||
			strings.HasPrefix(line, "We believe this is a bug") ||
			strings.HasPrefix(line, "Related issues:") ||
			strings.HasPrefix(line, "- http") ||
			strings.HasPrefix(line, "http") ||
			strings.HasPrefix(line, "Some additional details") ||
			strings.HasPrefix(line, "Please report it by opening an issue") ||
			strings.HasPrefix(line, "If you can, please attach") ||
			strings.HasPrefix(line, "Thank you for helping") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return strings.TrimSpace(out.String())
}

// redactUserinfo 把 http://user:pass@host/... 里的 userinfo 换成 ***。
func redactUserinfo(text string) string {
	var out strings.Builder
	rest := text
	for {
		scheme := strings.Index(rest, "://")
		if scheme < 0 {
			break
		}
		out.WriteString(rest[:scheme+3])
		after := rest[scheme+3:]
		at := strings.IndexByte(after, '@')
		if at >= 0 && !strings.Contains(after[:at], "/") && !strings.ContainsAny(after[:at], " \t\r\n") {
			out.WriteString("***@")
			rest = after[at+1:]
			continue
		}
		rest = after
	}
	out.WriteString(rest)
	return out.String()
}
