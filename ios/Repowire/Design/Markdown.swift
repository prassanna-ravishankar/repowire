import SwiftUI

extension Text {
    /// Inline markdown (code spans, emphasis, links) as agents write it,
    /// keeping their line breaks.
    init(markdown: String) {
        let options = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        self.init((try? AttributedString(markdown: markdown, options: options)) ?? AttributedString(markdown))
    }
}
