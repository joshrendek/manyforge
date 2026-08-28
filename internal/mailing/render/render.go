// Package render turns mailing Markdown into a safe, shared email layout and
// performs recipient-specific substitution and tracking as a separate pass.
package render

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strings"

	"github.com/inbucket/html2text"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"
)

const unsubscribeMarker = "__MF_UNSUBSCRIBE_URL__"

//go:embed templates/layout.html
var layoutSource string

var variablePattern = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

// Input contains campaign-stable Markdown and layout fields to compile.
type Input struct {
	BodyMarkdown  string
	FromName      string
	Preheader     string
	PostalAddress string
}

// Variables contains the supported recipient substitutions: first_name,
// last_name, email, unsubscribe_url, and list_name.
type Variables struct {
	FirstName      string
	LastName       string
	Email          string
	UnsubscribeURL string
	ListName       string
}

// Tracking keeps token creation outside the renderer. A nil ClickURL disables
// click rewriting; an empty OpenURL disables the tracking pixel.
type Tracking struct {
	ClickURL func(destination string) (string, error)
	OpenURL  string
}

// Compiled is campaign-stable HTML that can be reused across recipients.
type Compiled struct{ HTML string }

// Output contains the final recipient-specific HTML and generated plain text.
type Output struct {
	HTML string `json:"html"`
	Text string `json:"text"`
}

// Renderer compiles safe Markdown and renders recipient-specific output. It is
// safe for concurrent use after construction.
type Renderer struct {
	markdown goldmark.Markdown
	layout   *template.Template
}

// New constructs a renderer using the package's embedded email layout.
func New() (*Renderer, error) {
	t, err := template.New("mailing-layout").Parse(layoutSource)
	if err != nil {
		return nil, fmt.Errorf("mailing render: parse layout: %w", err)
	}
	return &Renderer{
		// Deliberately do not set html.WithUnsafe. Goldmark therefore replaces
		// raw HTML blocks with comments rather than trusting author-supplied HTML.
		markdown: goldmark.New(goldmark.WithExtensions(extension.GFM)),
		layout:   t,
	}, nil
}

// Compile performs the campaign/template-stable half of rendering. The
// resulting HTML can be cached and safely reused for multiple recipients.
func (r *Renderer) Compile(in Input) (Compiled, error) {
	var body bytes.Buffer
	if err := r.markdown.Convert([]byte(in.BodyMarkdown), &body); err != nil {
		return Compiled{}, fmt.Errorf("mailing render: markdown: %w", err)
	}
	var page bytes.Buffer
	data := struct {
		FromName, Preheader, PostalAddress string
		Body                               template.HTML
		UnsubscribeMarker                  string
	}{
		FromName: in.FromName, Preheader: in.Preheader,
		PostalAddress: in.PostalAddress,
		// Goldmark emitted this fragment with unsafe HTML disabled. Marking only
		// that output trusted lets the layout preserve headings and links while
		// all profile fields continue through html/template escaping.
		Body:              template.HTML(body.String()), // #nosec G203 -- trusted renderer output, not raw author HTML.
		UnsubscribeMarker: unsubscribeMarker,
	}
	if err := r.layout.Execute(&page, data); err != nil {
		return Compiled{}, fmt.Errorf("mailing render: execute layout: %w", err)
	}
	return Compiled{HTML: page.String()}, nil
}

// Render performs the recipient-specific half: escaped variables, safe link
// normalization/rewriting, optional open pixel, and generated plain text.
func (r *Renderer) Render(compiled Compiled, vars Variables, tracking Tracking) (Output, error) {
	values := map[string]string{
		"first_name": vars.FirstName, "last_name": vars.LastName,
		"email": vars.Email, "unsubscribe_url": vars.UnsubscribeURL,
		"list_name": vars.ListName,
	}
	htmlText := strings.ReplaceAll(compiled.HTML, unsubscribeMarker, template.HTMLEscapeString(vars.UnsubscribeURL))
	htmlText = variablePattern.ReplaceAllStringFunc(htmlText, func(match string) string {
		parts := variablePattern.FindStringSubmatch(match)
		return template.HTMLEscapeString(values[parts[1]])
	})

	doc, err := html.Parse(strings.NewReader(htmlText))
	if err != nil {
		return Output{}, fmt.Errorf("mailing render: parse html: %w", err)
	}
	if err := rewriteLinks(doc, vars.UnsubscribeURL, tracking.ClickURL); err != nil {
		return Output{}, err
	}
	if tracking.OpenURL != "" {
		appendOpenPixel(doc, tracking.OpenURL)
	}
	var rendered bytes.Buffer
	if err := html.Render(&rendered, doc); err != nil {
		return Output{}, fmt.Errorf("mailing render: serialize html: %w", err)
	}
	plain, err := html2text.FromString(rendered.String())
	if err != nil {
		return Output{}, fmt.Errorf("mailing render: plain text: %w", err)
	}
	return Output{HTML: rendered.String(), Text: strings.TrimSpace(plain)}, nil
}

// RenderInput composes Compile and Render for callers that do not cache compiled output.
func (r *Renderer) RenderInput(in Input, vars Variables, tracking Tracking) (Output, error) {
	compiled, err := r.Compile(in)
	if err != nil {
		return Output{}, err
	}
	return r.Render(compiled, vars, tracking)
}

func rewriteLinks(n *html.Node, unsubscribeURL string, clickURL func(string) (string, error)) error {
	if n.Type == html.ElementNode && n.Data == "a" {
		for i := range n.Attr {
			if n.Attr[i].Key != "href" {
				continue
			}
			href := strings.TrimSpace(n.Attr[i].Val)
			if href == "" || strings.HasPrefix(href, "#") || strings.EqualFold(href, unsubscribeURL) {
				break
			}
			u, err := url.Parse(href)
			if err != nil {
				n.Attr[i].Val = ""
				break
			}
			switch strings.ToLower(u.Scheme) {
			case "mailto":
				// Mail links are intentionally usable but never tracked.
			case "http", "https":
				if clickURL != nil {
					rewritten, err := clickURL(href)
					if err != nil {
						return fmt.Errorf("mailing render: rewrite link: %w", err)
					}
					n.Attr[i].Val = rewritten
				}
			default:
				// Relative and active-content schemes are stripped. Campaign mail
				// has no trustworthy base URL for resolving relative destinations.
				n.Attr[i].Val = ""
			}
			break
		}
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if err := rewriteLinks(child, unsubscribeURL, clickURL); err != nil {
			return err
		}
	}
	return nil
}

func appendOpenPixel(doc *html.Node, src string) {
	body := findElement(doc, "body")
	if body == nil {
		return
	}
	body.AppendChild(&html.Node{Type: html.ElementNode, Data: "img", Attr: []html.Attribute{
		{Key: "src", Val: src}, {Key: "width", Val: "1"}, {Key: "height", Val: "1"},
		{Key: "alt", Val: ""}, {Key: "style", Val: "display:none!important"},
	}})
}

func findElement(n *html.Node, name string) *html.Node {
	if n.Type == html.ElementNode && n.Data == name {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findElement(child, name); found != nil {
			return found
		}
	}
	return nil
}
