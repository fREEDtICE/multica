package provision

import (
	"bytes"
	"encoding/xml"
	"io"
)

// xmlNode is a minimal, namespace-agnostic XML tree used to read the subset
// of EC2 query responses the provisioner needs.
type xmlNode struct {
	Name     string
	Text     string
	Children []*xmlNode
}

func parseXML(data []byte) (*xmlNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	root := &xmlNode{}
	stack := []*xmlNode{root}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if len(root.Children) == 0 {
				return nil, nil
			}
			return root.Children[0], nil
		}
		if err != nil {
			return nil, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			node := &xmlNode{Name: t.Name.Local}
			parent := stack[len(stack)-1]
			parent.Children = append(parent.Children, node)
			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(t)
			}
		}
	}
}

// first returns the first descendant reached by the local-name path.
func (n *xmlNode) first(path ...string) *xmlNode {
	if n == nil || len(path) == 0 {
		return n
	}
	for _, child := range n.Children {
		if child.Name == path[0] {
			if found := child.first(path[1:]...); found != nil {
				return found
			}
		}
	}
	return nil
}

// findAll returns every descendant with the given local name at any depth.
func (n *xmlNode) findAll(name string) []*xmlNode {
	var out []*xmlNode
	if n == nil {
		return out
	}
	for _, child := range n.Children {
		if child.Name == name {
			out = append(out, child)
		}
		out = append(out, child.findAll(name)...)
	}
	return out
}

func (n *xmlNode) text(path ...string) string {
	if found := n.first(path...); found != nil {
		return found.Text
	}
	return ""
}

// deepText returns the text of the first descendant with the given local name.
func (n *xmlNode) deepText(name string) string {
	if found := n.findAll(name); len(found) > 0 {
		return found[0].Text
	}
	return ""
}
