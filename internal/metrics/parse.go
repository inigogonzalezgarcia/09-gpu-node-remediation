// Package metrics reads the Prometheus text exposition format, which is what
// NVIDIA dcgm-exporter (and the lab simulator) serve on /metrics.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Sample is one line of a metrics page: name{labels} value.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Parse reads every sample from r. Comments (# HELP, # TYPE) and blank lines are skipped.
func Parse(r io.Reader) ([]Sample, error) {
	var out []Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		s, err := parseLine(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

func parseLine(text string) (Sample, error) {
	s := Sample{Labels: map[string]string{}}
	rest := text
	if i := strings.IndexByte(text, '{'); i >= 0 {
		j := strings.LastIndexByte(text, '}')
		if j < i {
			return s, fmt.Errorf("unbalanced braces")
		}
		s.Name = text[:i]
		labels, err := parseLabels(text[i+1 : j])
		if err != nil {
			return s, err
		}
		s.Labels = labels
		rest = strings.TrimSpace(text[j+1:])
	} else {
		fields := strings.Fields(text)
		if len(fields) < 2 {
			return s, fmt.Errorf("missing value")
		}
		s.Name = fields[0]
		rest = strings.Join(fields[1:], " ")
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return s, fmt.Errorf("missing value")
	}
	v, err := strconv.ParseFloat(fields[0], 64) // a trailing timestamp, if any, is ignored
	if err != nil {
		return s, fmt.Errorf("bad value %q", fields[0])
	}
	s.Value = v
	return s, nil
}

func parseLabels(body string) (map[string]string, error) {
	labels := map[string]string{}
	for len(strings.TrimSpace(body)) > 0 {
		body = strings.TrimLeft(body, " ,")
		eq := strings.IndexByte(body, '=')
		if eq < 0 {
			return nil, fmt.Errorf("bad label in %q", body)
		}
		key := strings.TrimSpace(body[:eq])
		body = body[eq+1:]
		if !strings.HasPrefix(body, `"`) {
			return nil, fmt.Errorf("label %s: value must be quoted", key)
		}
		var b strings.Builder
		i := 1
		for ; i < len(body); i++ {
			c := body[i]
			if c == '\\' && i+1 < len(body) {
				i++
				switch body[i] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(body[i])
				}
				continue
			}
			if c == '"' {
				break
			}
			b.WriteByte(c)
		}
		if i >= len(body) {
			return nil, fmt.Errorf("label %s: unterminated value", key)
		}
		labels[key] = b.String()
		body = body[i+1:]
	}
	return labels, nil
}
