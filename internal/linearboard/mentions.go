package linearboard

import (
	"errors"
	"regexp"
)

// errMention is the public refusal for agent-written text that mentions
// someone in Linear. Nothing is sent.
var errMention = errors.New("mentions are not allowed in agent-written text; nothing was sent. Rephrase without @: name people and agents in plain words, and write scoped packages without it (types/node)")

// mentionPatterns are refused anywhere in agent-written text, code spans and
// fences included: Linear's markdown may not render code the way CommonMark
// does, so structure is never trusted to hide a mention.
//   - an @handle (Linear's documented mention syntax, covering @Linear, its
//     agent) unless a letter or digit comes right before the @, as in an
//     email address (a@linear.app); "Done.@bob" is a mention;
//   - a link to a Linear profile, or a mention:// link, the encodings a
//     mention may take in markdown.
var mentionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?:\A|[^A-Za-z0-9])@[\p{L}\p{N}_]`),
	regexp.MustCompile(`(?i)linear\.app/(?:[^\s/)\]>"']+/)?profiles\b`),
	regexp.MustCompile(`(?i)\bmention://`),
}

// mentionGuard refuses any of texts that mentions a Linear user, app or
// agent. It runs in validation, before the ledger and before dispatch.
func mentionGuard(texts ...*string) error {
	for _, t := range texts {
		if t == nil {
			continue
		}
		for _, pattern := range mentionPatterns {
			if pattern.MatchString(*t) {
				return errMention
			}
		}
	}
	return nil
}
