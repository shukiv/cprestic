package webui

import (
	"regexp"
	"strings"
	"testing"
)

// WHM's own stylesheets own thousands of short class names — .actions,
// .btn, .search — and they reach a plugin fragment because it renders
// inside WHM's page. A plugin that borrows one of those names inherits
// its styling, which is how every row in the account table came to have a
// grey box behind its buttons. Every class we render therefore carries the
// cpr- prefix, and this test is what keeps a new one from slipping in.
const classPrefix = "cpr-"

// wrapperClass scopes our own stylesheet and is deliberately unprefixed.
const wrapperClass = "gniza"

var (
	classAttr   = regexp.MustCompile(`class="([^"]*)"`)
	action      = regexp.MustCompile(`(?s){{.*?}}`)
	cssSelector = regexp.MustCompile(`(?s)([^{}]+)\{`)
	cssComment  = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

func TestEveryRenderedClassIsNamespaced(t *testing.T) {
	files, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	for _, f := range files {
		body, err := templateFS.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatalf("read %s: %v", f.Name(), err)
		}
		// Actions come out first: one of them holds a quoted string, and
		// a class attribute read around it ends in the wrong place.
		markup := action.ReplaceAllString(string(body), " ")
		for _, m := range classAttr.FindAllStringSubmatch(markup, -1) {
			for _, name := range strings.Fields(m[1]) {
				if name == wrapperClass || strings.HasPrefix(name, classPrefix) {
					continue
				}
				t.Errorf("%s: class %q is not namespaced — WHM may already own it, prefix it with %q",
					f.Name(), name, classPrefix)
			}
		}
	}
}

func TestStylesheetOnlyTargetsNamespacedClasses(t *testing.T) {
	body, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	// Comments carry filenames — app.css, README.md — and a dotted name
	// reads as a class to anything scanning selectors.
	stylesheet := cssComment.ReplaceAllString(string(body), " ")
	for _, m := range cssSelector.FindAllStringSubmatch(stylesheet, -1) {
		for _, name := range classNames(m[1]) {
			if name == wrapperClass || strings.HasPrefix(name, classPrefix) {
				continue
			}
			t.Errorf("selector %q targets class %q, which is not namespaced",
				strings.TrimSpace(m[1]), name)
		}
	}
}

// classNames pulls the class names out of a selector, skipping decimals so
// that a value like 0.5rem is not mistaken for a class.
func classNames(selector string) []string {
	var names []string
	for i := 0; i < len(selector); i++ {
		if selector[i] != '.' {
			continue
		}
		if i > 0 && isWordByte(selector[i-1]) && isDigitByte(selector[i-1]) {
			continue
		}
		j := i + 1
		if j >= len(selector) || !isNameStart(selector[j]) {
			continue
		}
		for j < len(selector) && isWordByte(selector[j]) {
			j++
		}
		names = append(names, selector[i+1:j])
		i = j - 1
	}
	return names
}

func isWordByte(b byte) bool {
	return b == '_' || b == '-' || (b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

func isNameStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

var (
	templateAction = regexp.MustCompile(`{{.*?}}`)
	midLink        = regexp.MustCompile(`href="\?[^"]*&amp;{{\s*\$`)
)

// A link built in two halves is escaped as one value.
//
// html/template escapes what goes into an href by where it lands. A whole
// URL is normalised, leaving its ampersands and equals signs alone; a value
// spliced in after the query has begun is escaped as one query value, so a
// prefix carrying its own parameters arrives as a single unreadable one and
// everything after the first parameter is lost. And a prefix built with
// printf is escaped once on the way out, so "&amp;" written there arrives
// as "&amp;amp;" and names a parameter nobody reads.
//
// Both faults render a page that looks right and links nowhere, which is
// why they are caught here rather than by eye.
func TestALinkBuiltFromAPrefixStillCarriesItsParameters(t *testing.T) {
	files, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	for _, f := range files {
		body, err := templateFS.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatalf("read %s: %v", f.Name(), err)
		}
		for _, action := range templateAction.FindAllString(string(body), -1) {
			if strings.Contains(action, "printf") && strings.Contains(action, "&amp;") {
				t.Errorf("%s: a printf-built link writes &amp; where a raw & is meant: %s",
					f.Name(), action)
			}
		}
		if found := midLink.FindString(string(body)); found != "" {
			t.Errorf("%s: a link prefix is spliced into the middle of a query: %s",
				f.Name(), found)
		}
	}
}
