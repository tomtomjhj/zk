package extensions

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// represents a block id
type BlockId struct {
	gast.BaseInline
	ID string
}

func (n *BlockId) Dump(source []byte, level int) {
	m := map[string]string{}
	m["BlockId"] = n.ID
	gast.DumpHelper(n, source, level, m, nil)
}

var KindBlockId = gast.NewNodeKind("BlockId")

func (n *BlockId) Kind() gast.NodeKind {
	return KindBlockId
}

type BlockIdExt struct{}

func (t *BlockIdExt) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithInlineParsers(
			util.Prioritized(&blockIdParser{}, 2000),
		),
	)
}

// Obsidian:
// * alphanumeric (it says "letters" but it doesn't mean unicode [L]etter) and hyphen-minus
// * preceded by nothing (start of line) or a space (other whitespaces are not allowed)
// * followed by nothing, i.e. the end of parent block
type blockIdParser struct{}

func (p *blockIdParser) Trigger() []byte {
	return []byte{'^'}
}

func (p *blockIdParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()

	if !(before == ' ' || before == '\n') {
		return nil
	}

	line, _ := block.PeekLine()
	var id string // Accumulator for the block id
	var advance int

	for i, char := range string(line) {
		// skip ^
		if i == 0 {
			continue
		}

		if !isValidBlockIdChar(char) {
			break
		}

		id += string(char)
		advance = i + 1
	}

	if len(id) == 0 {
		return nil
	}

	// not followed by anything else
	parentSegments := parent.Lines()
	parentLastLine := parentSegments.Len() - 1
	parentStop := parentSegments.At(parentLastLine).Stop
	blockLine, blockSegment := block.Position()
	candidateStop := blockSegment.Start + advance
	if !(parentLastLine == blockLine && parentStop == candidateStop) {
		return nil
	}

	block.Advance(advance)
	return &BlockId{
		BaseInline: gast.BaseInline{},
		ID:         id,
	}

}

func isValidBlockIdChar(r rune) bool {
	return ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') || r == '-'
}
