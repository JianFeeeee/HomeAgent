/**
 * Static Markdown → ArkUI renderer for COMPLETE (non-streaming) chat messages.
 *
 * Parses once in aboutToAppear, builds a component tree with no timers — instant rendering.
 * Uses Span children inside Text for inline bold/italic/code/link formatting.
 *
 * Covers: headings, paragraphs, code fences, unordered/ordered lists,
 * blockquotes, horizontal rules, tables, and inline bold/italic/code/links.
 */

import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';
import { RADIUS_SM } from '../common/Constants';

// ── Types ──────────────────────────────────────────────────────────────────────

export interface MdBlock {
  type: string; // 'heading' | 'code' | 'list' | 'ol' | 'blockquote' | 'hr' | 'table' | 'para'
  level?: number;
  items?: string[];
  text?: string;
  lang?: string;
  codeLines?: string[];
  headers?: string[];
  rows?: string[][];
}

export interface MdSpan {
  text: string;
  bold?: boolean;
  italic?: boolean;
  code?: boolean;
  link?: boolean;
  linkUrl?: string;
}

// ── Inline parser ──────────────────────────────────────────────────────────────

export function parseInline(text: string): MdSpan[] {
  const spans: MdSpan[] = [];
  let i: number = 0;
  while (i < text.length) {
    // Inline code (backtick)
    if (text[i] === '`') {
      const end: number = text.indexOf('`', i + 1);
      if (end > i) {
        spans.push({ text: text.substring(i + 1, end), code: true });
        i = end + 1;
        continue;
      }
    }
    // Bold: **text**
    if (text[i] === '*' && i + 1 < text.length && text[i + 1] === '*') {
      const end: number = text.indexOf('**', i + 2);
      if (end > i + 1) {
        spans.push({ text: text.substring(i + 2, end), bold: true });
        i = end + 2;
        continue;
      }
    }
    // Italic: *text* (single asterisk)
    if (text[i] === '*' && (i + 1 >= text.length || text[i + 1] !== '*')) {
      const end: number = text.indexOf('*', i + 1);
      if (end > i) {
        spans.push({ text: text.substring(i + 1, end), italic: true });
        i = end + 1;
        continue;
      }
    }
    // Link: [text](url)
    if (text[i] === '[') {
      const cb: number = text.indexOf(']', i + 1);
      if (cb > i && cb + 1 < text.length && text[cb + 1] === '(') {
        const cp: number = text.indexOf(')', cb + 2);
        if (cp > cb + 1) {
          spans.push({ text: text.substring(i + 1, cb), link: true, linkUrl: text.substring(cb + 2, cp) });
          i = cp + 1;
          continue;
        }
      }
    }
    // Plain run
    let j: number = i + 1;
    while (j < text.length && text[j] !== '`' && text[j] !== '*' && text[j] !== '[') {
      j++;
    }
    spans.push({ text: text.substring(i, j) });
    i = j;
  }
  return spans;
}

// ── Block parser helpers ───────────────────────────────────────────────────────

function isHr(line: string): boolean {
  if (line.length < 3) {
    return false;
  }
  const ch: string = line[0];
  if (ch !== '-' && ch !== '*' && ch !== '_') {
    return false;
  }
  for (let k = 0; k < line.length; k++) {
    if (line[k] !== ch) {
      return false;
    }
  }
  return true;
}

function isOlStart(line: string): boolean {
  if (line.length < 3) {
    return false;
  }
  let k: number = 0;
  while (k < line.length && line[k] >= '0' && line[k] <= '9') {
    k++;
  }
  return k > 0 && k + 1 < line.length && line[k] === '.' && line[k + 1] === ' ';
}

function isTableSep(line: string): boolean {
  if (!line.includes('-')) {
    return false;
  }
  for (let k = 0; k < line.length; k++) {
    const c: string = line[k];
    if (c !== '|' && c !== '-' && c !== ':' && c !== ' ' && c !== '\t') {
      return false;
    }
  }
  return true;
}

// ── Block parser ───────────────────────────────────────────────────────────────

export function parseBlocks(content: string): MdBlock[] {
  if (content.length === 0) {
    return [];
  }
  const lines: string[] = content.split('\n');
  const blocks: MdBlock[] = [];
  let i: number = 0;

  while (i < lines.length) {
    const line: string = lines[i];

    // Empty line
    if (line.trim().length === 0) {
      i++;
      continue;
    }

    // Code fence
    if (line.startsWith('```')) {
      const langEnd: number = line.indexOf('`', 3);
      const lang: string = langEnd > 3 ? line.substring(3, langEnd).trim() : '';
      const codeLines: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith('```')) {
        codeLines.push(lines[i]);
        i++;
      }
      if (i < lines.length) {
        i++;
      }
      blocks.push({ type: 'code', lang: lang, codeLines: codeLines });
      continue;
    }

    // Heading
    if (line.startsWith('#')) {
      let level: number = 0;
      while (level < line.length && line[level] === '#') {
        level++;
      }
      if (level <= 6 && level < line.length && line[level] === ' ') {
        blocks.push({ type: 'heading', level: level, text: line.substring(level + 1).trim() });
        i++;
        continue;
      }
    }

    // Horizontal rule
    if (isHr(line.trim())) {
      blocks.push({ type: 'hr' });
      i++;
      continue;
    }

    // Unordered list
    if ((line.startsWith('- ') || line.startsWith('* ')) && !line.startsWith('- [')) {
      const items: string[] = [];
      while (i < lines.length && (lines[i].startsWith('- ') || lines[i].startsWith('* ')) && !lines[i].startsWith('- [')) {
        items.push(lines[i].substring(2));
        i++;
      }
      blocks.push({ type: 'list', items: items });
      continue;
    }

    // Ordered list
    if (isOlStart(line)) {
      const items: string[] = [];
      while (i < lines.length && isOlStart(lines[i])) {
        const dotIdx: number = lines[i].indexOf('. ');
        items.push(lines[i].substring(dotIdx + 2));
        i++;
      }
      blocks.push({ type: 'ol', items: items });
      continue;
    }

    // Blockquote
    if (line.startsWith('> ')) {
      const qLines: string[] = [];
      while (i < lines.length && lines[i].startsWith('> ')) {
        qLines.push(lines[i].substring(2));
        i++;
      }
      blocks.push({ type: 'blockquote', text: qLines.join('\n') });
      continue;
    }

    // Table
    if (line.trimStart().startsWith('|') && !isTableSep(line)) {
      const tLines: string[] = [];
      while (i < lines.length && lines[i].trimStart().startsWith('|')) {
        tLines.push(lines[i]);
        i++;
      }
      if (tLines.length >= 2) {
        const parseRow = (row: string): string[] => {
          const cells: string[] = [];
          const parts: string[] = row.split('|');
          for (let p = 0; p < parts.length; p++) {
            const c: string = parts[p].trim();
            if (c.length > 0) {
              cells.push(c);
            }
          }
          return cells;
        };
        const headers: string[] = parseRow(tLines[0]);
        const rows: string[][] = [];
        for (let k = 1; k < tLines.length; k++) {
          if (!isTableSep(tLines[k].trim())) {
            rows.push(parseRow(tLines[k]));
          }
        }
        if (headers.length > 0) {
          blocks.push({ type: 'table', headers: headers, rows: rows });
        }
      }
      continue;
    }

    // Paragraph: collect consecutive non-special lines
    {
      const paraLines: string[] = [];
      while (i < lines.length) {
        const ln: string = lines[i];
        if (ln.trim().length === 0) {
          break;
        }
        if (ln.startsWith('```') || ln.startsWith('#') || isHr(ln.trim())) {
          break;
        }
        if (ln.startsWith('- ') || ln.startsWith('* ') || isOlStart(ln) || ln.startsWith('> ')) {
          break;
        }
        if (ln.trimStart().startsWith('|') && !isTableSep(ln)) {
          break;
        }
        paraLines.push(ln);
        i++;
      }
      if (paraLines.length > 0) {
        blocks.push({ type: 'para', text: paraLines.join('\n') });
      }
    }
  }
  return blocks;
}

// ── Component ──────────────────────────────────────────────────────────────────

@Component
export struct StaticMarkdownView {
  @Prop content: string = '';
  @Prop isDark: boolean = true;
  private blocks: MdBlock[] = [];

  aboutToAppear(): void {
    this.blocks = parseBlocks(this.content);
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  private headingSize(level: number): number {
    if (level === 1) {
      return 22;
    }
    if (level === 2) {
      return 19;
    }
    if (level === 3) {
      return 17;
    }
    if (level === 4) {
      return 15.5;
    }
    return 14;
  }

  // ── Block builders ─────────────────────────────────────────────────────────

  @Builder
  ParaBlock(text: string) {
    Column({ space: 1 }) {
      ForEach(this.splitNewlines(text), (ln: string, idx: number) => {
        // Render inline spans inside this line
        Text() {
          ForEach(parseInline(ln), (sp: MdSpan, si: number) => {
            Span(sp.text)
              .fontSize(15)
              .fontColor(sp.link === true ? this.palette().accent : sp.code === true ? this.palette().preText : this.palette().msgBubbleText)
              .fontWeight(sp.bold === true ? FontWeight.Bold : FontWeight.Normal)
              .fontStyle(sp.italic === true ? FontStyle.Italic : FontStyle.Normal)
              .fontFamily(sp.code === true ? 'monospace' : '-')
              .backgroundColor(sp.code === true ? this.palette().preBg : Color.Transparent)
              .borderRadius(3)
              .padding(sp.code === true ? { left: 3, right: 3, top: 1, bottom: 1 } : {})
              .decoration(sp.link === true ? { type: TextDecorationType.Underline } : undefined)
          }, (sp: MdSpan, si: number) => idx.toString() + '_' + si.toString())
        }
        .fontSize(15)
        .lineHeight(24)
        .fontColor(this.palette().msgBubbleText)
        .width('100%')
        .wordBreak(WordBreak.BREAK_ALL)
        .textAlign(TextAlign.Start)
      }, (ln: string, idx: number) => 'p' + idx.toString())
    }
    .width('100%')
    .margin({ top: 2, bottom: 4 })
  }

  @Builder
  HeadingBlock(block: MdBlock) {
    Text(block.text ?? '')
      .fontSize(this.headingSize(block.level ?? 1))
      .fontWeight(FontWeight.Bold)
      .fontColor(this.palette().textPrimary)
      .lineHeight(this.headingSize(block.level ?? 1) + 8)
      .width('100%')
      .margin({ top: 6, bottom: 4 })
  }

  @Builder
  CodeBlock(block: MdBlock) {
    Column() {
      Row() {
        Text(block.lang !== undefined && block.lang.length > 0 ? block.lang : 'code')
          .fontSize(10)
          .fontColor(this.palette().textMuted)
          .fontFamily('monospace')
        Blank()
      }
      .width('100%')
      .padding({ left: 10, right: 10, top: 5, bottom: 5 })
      .backgroundColor(this.palette().bgHover)

      // Code body
      Column() {
        ForEach(block.codeLines ?? [], (ln: string, idx: number) => {
          Text(ln.length > 0 ? ln : ' ')
            .fontSize(12.5)
            .lineHeight(19)
            .fontFamily('monospace')
            .fontColor(this.palette().preText)
            .width('100%')
            .textAlign(TextAlign.Start)
            .wordBreak(WordBreak.BREAK_ALL)
        }, (ln: string, idx: number) => 'c' + idx.toString())
      }
      .width('100%')
      .padding({ left: 10, right: 10, top: 8, bottom: 8 })
    }
    .width('100%')
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().preBg)
    .border({ width: 1, color: this.palette().kvBorder })
    .clip(true)
    .margin({ top: 4, bottom: 6 })
  }

  @Builder
  ListBlock(block: MdBlock) {
    Column({ space: 2 }) {
      ForEach(block.items ?? [], (item: string, idx: number) => {
        Row({ space: 6 }) {
          Text('•')
            .fontSize(15)
            .fontColor(this.palette().accent)
            .fontWeight(FontWeight.Bold)
            .margin({ top: 1 })
          Text() {
            ForEach(parseInline(item), (sp: MdSpan, si: number) => {
              Span(sp.text)
                .fontSize(15)
                .fontColor(sp.code === true ? this.palette().preText : this.palette().msgBubbleText)
                .fontWeight(sp.bold === true ? FontWeight.Bold : FontWeight.Normal)
                .fontStyle(sp.italic === true ? FontStyle.Italic : FontStyle.Normal)
                .fontFamily(sp.code === true ? 'monospace' : '-')
                .backgroundColor(sp.code === true ? this.palette().preBg : Color.Transparent)
                .borderRadius(3)
                .padding(sp.code === true ? { left: 3, right: 3, top: 1, bottom: 1 } : {})
            }, (sp: MdSpan, si: number) => 'li' + idx.toString() + '_' + si.toString())
          }
          .fontSize(15)
          .lineHeight(23)
          .fontColor(this.palette().msgBubbleText)
          .layoutWeight(1)
          .wordBreak(WordBreak.BREAK_ALL)
          .width('100%')
        }
        .width('100%')
        .alignItems(VerticalAlign.Top)
      }, (item: string, idx: number) => idx.toString())
    }
    .width('100%')
    .margin({ top: 2, bottom: 4 })
  }

  @Builder
  OlBlock(block: MdBlock) {
    Column({ space: 2 }) {
      ForEach(block.items ?? [], (item: string, idx: number) => {
        Row({ space: 6 }) {
          Text((idx + 1).toString() + '.')
            .fontSize(15)
            .fontColor(this.palette().accent)
            .fontWeight(FontWeight.Medium)
            .margin({ top: 1 })
          Text() {
            ForEach(parseInline(item), (sp: MdSpan, si: number) => {
              Span(sp.text)
                .fontSize(15)
                .fontColor(sp.code === true ? this.palette().preText : this.palette().msgBubbleText)
                .fontWeight(sp.bold === true ? FontWeight.Bold : FontWeight.Normal)
                .fontStyle(sp.italic === true ? FontStyle.Italic : FontStyle.Normal)
                .fontFamily(sp.code === true ? 'monospace' : '-')
                .backgroundColor(sp.code === true ? this.palette().preBg : Color.Transparent)
                .borderRadius(3)
                .padding(sp.code === true ? { left: 3, right: 3, top: 1, bottom: 1 } : {})
            }, (sp: MdSpan, si: number) => 'oli' + idx.toString() + '_' + si.toString())
          }
          .fontSize(15)
          .lineHeight(23)
          .fontColor(this.palette().msgBubbleText)
          .layoutWeight(1)
          .wordBreak(WordBreak.BREAK_ALL)
          .width('100%')
        }
        .width('100%')
        .alignItems(VerticalAlign.Top)
      }, (item: string, idx: number) => idx.toString())
    }
    .width('100%')
    .margin({ top: 2, bottom: 4 })
  }

  @Builder
  BlockquoteBlock(text: string) {
    Column() {
      ForEach(this.splitNewlines(text), (ln: string, idx: number) => {
        Text() {
          ForEach(parseInline(ln), (sp: MdSpan, si: number) => {
            Span(sp.text)
              .fontSize(14)
              .fontColor(this.palette().textTertiary)
              .fontStyle(sp.italic === true ? FontStyle.Italic : FontStyle.Normal)
              .fontWeight(sp.bold === true ? FontWeight.Bold : FontWeight.Normal)
          }, (sp: MdSpan, si: number) => 'bq' + idx.toString() + '_' + si.toString())
        }
        .fontSize(14)
        .lineHeight(22)
        .width('100%')
        .wordBreak(WordBreak.BREAK_ALL)
      }, (ln: string, idx: number) => 'bq' + idx.toString())
    }
    .width('100%')
    .padding({ left: 10, right: 10, top: 6, bottom: 6 })
    .margin({ top: 3, bottom: 5 })
    .borderRadius({ topLeft: 0, topRight: 6, bottomRight: 6, bottomLeft: 0 })
    .backgroundColor(this.palette().bgHover)
    .border({
      width: { left: 3, top: 0, right: 0, bottom: 0 },
      color: { left: this.palette().accent, top: Color.Transparent, right: Color.Transparent, bottom: Color.Transparent },
    })
  }

  @Builder
  HrBlock() {
    Row()
      .width('100%')
      .height(1)
      .backgroundColor(this.palette().kvBorder)
      .margin({ top: 6, bottom: 6 })
  }

  @Builder
  TableBlock(block: MdBlock) {
    Column() {
      // Header
      Row() {
        ForEach(block.headers ?? [], (h: string, hi: number) => {
          Text() {
            ForEach(parseInline(h), (sp: MdSpan, si: number) => {
              Span(sp.text)
                .fontSize(12.5)
                .fontWeight(FontWeight.Medium)
                .fontColor(this.palette().textPrimary)
            }, (sp: MdSpan, si: number) => 'th' + hi.toString() + '_' + si.toString())
          }
          .fontSize(12.5)
          .fontColor(this.palette().textPrimary)
          .layoutWeight(1)
          .padding({ left: 6, right: 6, top: 5, bottom: 5 })
        }, (h: string, hi: number) => 'th' + hi.toString())
      }
      .width('100%')
      .backgroundColor(this.palette().bgHover)

      // Body
      ForEach(block.rows ?? [], (row: string[], ri: number) => {
        Row() {
          ForEach(row, (cell: string, ci: number) => {
            Text() {
              ForEach(parseInline(cell), (sp: MdSpan, si: number) => {
                Span(sp.text)
                  .fontSize(12.5)
                  .fontColor(this.palette().msgBubbleText)
                  .fontFamily(sp.code === true ? 'monospace' : '-')
                  .backgroundColor(sp.code === true ? this.palette().preBg : Color.Transparent)
              }, (sp: MdSpan, si: number) => 'td' + ri.toString() + '_' + ci.toString() + '_' + si.toString())
            }
            .fontSize(12.5)
            .fontColor(this.palette().msgBubbleText)
            .layoutWeight(1)
            .padding({ left: 6, right: 6, top: 4, bottom: 4 })
            .wordBreak(WordBreak.BREAK_ALL)
            .width('100%')
          }, (cell: string, ci: number) => 'td' + ri.toString() + '_' + ci.toString())
        }
        .width('100%')
      }, (row: string[], ri: number) => 'tr' + ri.toString())
    }
    .width('100%')
    .borderRadius(RADIUS_SM)
    .border({ width: 1, color: this.palette().kvBorder })
    .clip(true)
    .margin({ top: 4, bottom: 6 })
  }

  // ── Helpers ────────────────────────────────────────────────────────────────

  private splitNewlines(text: string): string[] {
    if (text.length === 0) {
      return [];
    }
    return text.split('\n');
  }

  // ── Build ──────────────────────────────────────────────────────────────────

  build() {
    Column() {
      ForEach(this.blocks, (b: MdBlock, idx: number) => {
        if (b.type === 'code') {
          this.CodeBlock(b)
        } else if (b.type === 'heading') {
          this.HeadingBlock(b)
        } else if (b.type === 'list') {
          this.ListBlock(b)
        } else if (b.type === 'ol') {
          this.OlBlock(b)
        } else if (b.type === 'blockquote') {
          this.BlockquoteBlock(b.text ?? '')
        } else if (b.type === 'hr') {
          this.HrBlock()
        } else if (b.type === 'table') {
          this.TableBlock(b)
        } else {
          this.ParaBlock(b.text ?? '')
        }
      }, (b: MdBlock, idx: number) => idx.toString() + b.type + ((b.text ?? '').substring(0, 12)))
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
  }
}
