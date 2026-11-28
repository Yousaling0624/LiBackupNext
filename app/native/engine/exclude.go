package engine

import (
	"regexp"
	"strings"
)

// excludeMatcher 复刻 ignore crate 的 Override 语义（给 rustic 的 excludes.globs 用）：
//   - 普通 glob 是白名单（只保留匹配项）
//   - `!` 开头的 glob 是排除
//   - 后面的规则覆盖前面的
//   - 只要出现过白名单，没匹配上的**文件**默认排除；目录仍然放行以便向下找匹配项
//
// 项目里的模式要么是绝对路径（!/data/user/*/*/cache），要么是文件名/`**` 形式，
// 所以这里按「传给匹配器的是真实绝对路径」来实现。
type excludeMatcher struct {
	rules        []excludeRule
	hasWhitelist bool
}

type excludeRule struct {
	exclude bool
	dirOnly bool
	re      *regexp.Regexp
}

func newExcludeMatcher(patterns []string) *excludeMatcher {
	m := &excludeMatcher{}
	for _, raw := range patterns {
		raw = strings.TrimSpace(raw)
		if raw == "" || raw == "!" {
			continue
		}
		rule := excludeRule{}
		if strings.HasPrefix(raw, "!") {
			rule.exclude = true
			raw = raw[1:]
		} else {
			m.hasWhitelist = true
		}
		if strings.HasSuffix(raw, "/") {
			rule.dirOnly = true
			raw = strings.TrimRight(raw, "/")
		}
		if raw == "" {
			continue
		}
		re, err := compileGlob(raw)
		if err != nil {
			continue
		}
		rule.re = re
		m.rules = append(m.rules, rule)
	}
	return m
}

func (m *excludeMatcher) empty() bool {
	return m == nil || len(m.rules) == 0
}

// include 返回 true 表示保留。path 必须是真实绝对路径。
func (m *excludeMatcher) include(path string, isDir bool) bool {
	if m.empty() {
		return true
	}
	matched := false
	include := true
	for _, rule := range m.rules {
		if rule.dirOnly && !isDir {
			continue
		}
		if !rule.re.MatchString(path) {
			continue
		}
		matched = true
		include = !rule.exclude
	}
	if matched {
		return include
	}
	// 白名单模式下没命中的文件排除；目录放行，否则走不到里面的白名单文件。
	if m.hasWhitelist && !isDir {
		return false
	}
	return true
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	absolute := strings.HasPrefix(pattern, "/")
	hasSlash := strings.Contains(strings.TrimPrefix(pattern, "/"), "/")

	var b strings.Builder
	b.WriteByte('^')
	switch {
	case absolute:
		// 从真实绝对路径开头匹配
	case hasSlash:
		// 非绝对但带斜杠：相对任意层级（等价于前面补 **/）
		b.WriteString("(?:.*/)?")
	default:
		// 纯文件名：匹配任意层级里的 basename
		b.WriteString("(?:.*/)?")
	}

	bytes := []byte(pattern)
	for i := 0; i < len(bytes); i++ {
		c := bytes[i]
		switch c {
		case '*':
			if i+1 < len(bytes) && bytes[i+1] == '*' {
				i++
				if i+1 < len(bytes) && bytes[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := findClassEnd(bytes, i+1)
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := string(bytes[i : end+1])
			if strings.HasPrefix(class, "[!") {
				class = "[^" + class[2:]
			}
			b.WriteString(class)
			i = end
		case '\\':
			if i+1 < len(bytes) {
				i++
				b.WriteString(regexp.QuoteMeta(string(bytes[i])))
			} else {
				b.WriteString(`\\`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}

func findClassEnd(bytes []byte, start int) int {
	for i := start; i < len(bytes); i++ {
		if bytes[i] == '\\' {
			i++
			continue
		}
		if bytes[i] == ']' {
			return i
		}
	}
	return -1
}
