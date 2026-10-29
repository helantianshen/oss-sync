# OSS Sync Design System

This document records the visual system already implemented by the OSS Sync web console. Blog themes and the Obsidian plugin remain intentionally independent surfaces: blog themes own their template CSS, while plugin UI follows Obsidian's host styles.

## 1. Atmosphere & Identity

The console is a precise, utilitarian sync ledger: cool neutral surfaces, cobalt actions, compact metadata, and squared controls. Its signature is the visible grid canvas paired with ink-like offset shadows, making operational panels feel like physical records without becoming decorative. The Obsidian plugin is a denser companion surface that inherits the host theme instead of reproducing the web-console chrome.

The project mark is the connected OSS monogram in `internal/webui/assets/oss-logo.png`. Authentication pages, console navigation, the public directory and built-in blog fallbacks share `brand.css`; dark mode presents the same silhouette in white. Blog-provided logos keep precedence and their own sizing. The plugin embeds the mark in `src/brand.ts` for offline headers and settings. Browser and touch icons use the square `oss-icon.png` and multi-size `favicon.ico`; README images provide both light and dark variants.

## 2. Color

### Palette

| Role | Token | Light | Dark | Usage |
| --- | --- | --- | --- | --- |
| Canvas | `--canvas` | `#edf2f8` | `#0e1420` | Page background |
| Primary surface | `--paper` | `#ffffff` | `#161d2c` | Panels and controls |
| Primary text | `--ink` | `#10182b` | `#e6ebf5` | Headings and body |
| Secondary text | `--muted` | `#5d687d` | `#93a0b8` | Notes and metadata |
| Border | `--line` | `#cbd5e3` | `#2a3448` | Internal dividers |
| Strong border | `--line-dark` | `#9aa9be` | `#3d4a63` | Panel and control outlines |
| Primary action | `--cobalt` | `#35519c` | `#35519c` | Actions, buttons, and focus cues |
| Primary action hover | `--cobalt-dark` | `#29427f` | `#4565b8` | Hovered actions |
| Success | `--open` | `#087c61` | `#4fd1a5` | Approved and successful states |
| Danger | `--danger` | `#b33e48` | `#ff8e98` | Destructive and revoked states |
| Danger action | `--danger-button` | `#9f303b` | `#9f303b` | Destructive button fill with white-text contrast |
| Soft surface | `--surface-soft` | `#f7f9fd` | `#1a2234` | Hover and nested surfaces |
| Shared button surface | `--button-surface` | `#35519c` | `#35519c` | Shared default, primary, and compact button fill |
| Shared button text | `--button-ink` | `#ffffff` | `#ffffff` | Stable contrast on the cobalt button surface |
| Button shadow | `--button-shadow` | `#10182b` | `#000000` | Shared offset shadow for every button variant |

Accent colors communicate interaction or state, not decoration. New colors must be introduced as semantic custom properties before use.

## 3. Typography

### Scale

| Level | Size | Weight | Usage |
| --- | --- | --- | --- |
| Auth display | `clamp(38px, 5vw, 68px)` | 700+ | Authentication hero headings |
| Console page title | `clamp(38px, 5vw, 45px)` | 700+ | One `h1` per console page |
| Panel title | `clamp(28px, 4vw, 30px)` | 700+ | Panel `h2` headings |
| Body large | `17px` | 400 | Lead copy |
| Body | inherited browser `16px` | 400 | Forms and content |
| Navigation | `14px` | 700 | Primary navigation |
| Metadata | `10px` to `12px` | 700 | Labels, timestamps, and overlines |

Primary text uses `Aptos`, `Segoe UI`, and `Noto Sans SC`. Display headings use `Arial Narrow`, `Aptos Display`, and `Segoe UI`. Metadata uses `IBM Plex Mono` or `Cascadia Mono`.

## 4. Spacing & Layout

Spacing follows a 4px base rhythm, with established steps at 4, 8, 12, 16, 20, 24, 28, 32, 40, 48, and 64px. The main content width is capped at 1180px; authenticated console content uses a 264px fixed sidebar and a fluid `minmax(0, 1fr)` main column.

The document owns vertical scrolling. The sidebar and top bar remain sticky. Wide data tables own horizontal overflow through `.table-wrap`; grid children must keep `min-width: 0`. Console grids collapse to one column by 820px, and the sidebar becomes a drawer by 900px.

The Obsidian right sidebar uses the host pane as its scroll owner. Its internal rhythm is 4/8/12px, action clusters wrap before overflow, and status values truncate or wrap within the pane. Plugin CSS uses Obsidian variables for color, typography, borders, interactive surfaces, and radii.

## 5. Components

### Button

- **Structure**: `.button`, optionally combined with a semantic modifier.
- **Variants**: primary, danger, active, and compact theme control.
- **States**: default, hover, active selection, and `:focus-visible`.
- **Surface**: default, primary, and compact controls use the same `#35519c` cobalt fill in both themes; every variant uses `--button-shadow` so elevation never flips to a light shadow in dark mode.
- **Accessibility**: native button or link semantics; visible focus outline.
- **Layout**: inline cluster; controls retain at least a 34px target in compact groups.

### Theme switcher

- **Structure**: a three-button `role="group"` using `.button.theme-switcher__btn`.
- **States**: `aria-pressed` mirrors auto, light, or dark selection; `.is-active` carries the selected visual state.
- **Layout**: equal three-column grid in the sidebar and authentication masthead.

### Console sidebar navigation

- **Structure**: user-facing routes sit below a persistent `用户设置` section label. Administrators retain the complete user navigation and receive a separate `管理员设置` section containing only global administration routes.
- **Vault context**: Vault-scoped pages keep the `当前仓库` group open and show the active Vault name as a muted mono second line above its file, share, recycle, history, member, and settings links.
- **Iconography**: top-level entries use one consistent 24px outline SVG family at a 1.8px stroke. Icons are decorative (`aria-hidden`); every destination keeps a visible text label.
- **Responsive behavior**: desktop never shows drawer controls. Below 900px the top bar exposes a hamburger button and the drawer exposes a directional close button; links, backdrop, and Escape also close the drawer.

### Ledger panel and table

- **Structure**: `.ledger-panel` with a header and `.table-wrap` around semantic table markup.
- **States**: populated table or centered `.panel-empty` message.
- **Layout**: the table wrapper, not the page, owns horizontal overflow on narrow screens.

### Home metric grid

- **Structure**: the authenticated home page leads with an intrinsic grid of CPU, process-memory, vault, device, file, and pending-approval metrics above recent activity ledgers.
- **Navigation**: account-resource cards provide visible `进入` links to the corresponding vault or device management page; runtime-only cards remain informational.
- **Layout**: cards use `repeat(auto-fit, minmax(min(180px, 100%), 1fr))`, preserve the console's squared ledger material, and collapse without horizontal overflow at 375px.

### History filter bar

- **Structure**: the Vault history page exposes operation, username, device, start-time, and end-time controls in one semantic GET form above the ledger.
- **State**: submitted values remain visible; clearing filters preserves a path-scoped history view when present.
- **Layout**: five intrinsic filter columns plus actions on desktop, two columns below 820px, and one column below 520px.

### Device authorization

- **Structure**: a filtered device row in the admin table, carrying a compact one-line authorization summary and a trigger that opens a per-device modal with repo checkboxes and save/approve and revoke actions.
- **States**: pending, approved, revoked, and no-vault (every repository has been removed).
- **Accessibility**: the modal uses `role="dialog"` with an accessible label; Escape and backdrop click dismiss the modal and restore focus to the trigger; focus is trapped inside the open modal. Repository checkboxes sit in a labelled `fieldset` with a visible `legend`. Revoking an in-progress approval requires an explicit confirmation step.
- **Layout**: device names and identities truncate within the table row; the wrapping `.table-wrap` owns horizontal overflow. Actions in the modal form an inline, wrapping cluster.

### Collaboration authorization

- **Structure**: the Vault-scoped `协作成员` ledger groups accepted file collaborations by collaborator, shows an outer article count, and opens a focus-trapped detail dialog listing every authorized path.
- **Actions**: managers can revoke one article directly, select several with native checkboxes, select or clear all checkboxes, revoke the selected set, or revoke every accepted article for that collaborator.
- **Accessibility**: the article list is a labelled `fieldset`; the selected-action button remains disabled until at least one checkbox is selected; destructive actions require confirmation and modal focus returns to the opening control.
- **Layout**: article paths wrap within a two-column path/action row and collapse to one column below 520px; the modal owns vertical overflow for long authorization lists.

### Obsidian plugin sidebar

- **Structure**: a compact branded header with status summary and close action, followed by grouped status, actions, connection, bound-vault shares, collaboration, unresolved-conflicts, and console-link sections.
- **Entry**: a persistent left Ribbon button using Obsidian's registered `refresh-cw` icon opens or reveals the right sidebar.
- **States**: logged out, unbound, connected, syncing, error, empty invitations, and no unresolved conflicts.
- **Collaboration scope**: the bound-Vault ledger is merged with an authenticated account inbox so invitations from an owner's otherwise unknown Vault remain discoverable; every subsequent action uses the collaboration row's Vault identity. Account-level SSE carries all Vault collaboration events over HTTPS or loopback, while remote plaintext HTTP uses an account-level long poll that wakes on publication rather than timeout; legacy Vault polls receive compatibility wake-ups.
- **Accessibility**: icon-only close control has a localized accessible label and tooltip; buttons retain native Obsidian focus treatment.
- **Layout**: pane-local stack and wrapping action clusters; no fixed width and no nested vertical scrollbar.

### Plugin share ledger

- **Structure**: each bound-vault share shows its path, tabular view count, copy permission, and native actions for copying/opening the public URL, changing copy permission, and cancelling the share.
- **States**: unbound, loading, empty, populated, and request failure. Creating a share invalidates and reloads the ledger immediately.
- **Layout**: article paths wrap safely; actions form a two-column wrapping cluster inside the host sidebar width.

### Plugin management modal

- **Structure**: the sidebar exposes compact native buttons for share, collaboration, and recycle management; each opens a host-native modal with a vertically separated ledger.
- **States**: loading, empty, populated, request failure, and destructive confirmation. Management actions reload the active ledger immediately.
- **Accessibility**: native buttons retain Obsidian focus treatment; permanent deletion requires explicit confirmation and unavailable restore actions are disabled.
- **Layout**: the modal owns its content flow; paths use emergency wrapping, metadata remains secondary, and action clusters wrap before overflow.

### Vault template selector

- **Structure**: `仓库设置` lists built-in and administrator-published custom templates in a labelled native select.
- **Permissions**: a Vault owner or manager selects the template for that Vault. Uploading, scaffolding, editing, downloading, and deleting global template files remains administrator-only.
- **Sync policy boundary**: the same page shows the effective sync policy to every Vault manager, but only administrators receive an enabled policy selector; the server ignores forged policy fields from non-admin forms.
- **State**: the selected `ThemeName` persists on `VaultSetting`; navigation refreshes against enabled plugins associated with the selected template.
- **Functional settings**: templates do not declare or own settings. A linked plugin declares settings, stores values per Vault, and supplies safe values to the template through `.ThemeConfigJS`.

### Server console theme management

- **Administration**: `服务器主题` mirrors the template-management ledger with ZIP upload, copy-from-existing, prefilled online text editing, download, guarded deletion, and a Markdown guide in the shared focus-trapped modal primitive.
- **User choice**: ordinary users only receive a labelled native selector in `个人中心`. Their selected `ConsoleThemeName` is loaded after the base console stylesheet and never grants access to management routes.
- **Package contract**: `theme.css` is required. Common raster images and font files may be served from `/ui/themes/<name>/…`; HTML and JavaScript are not executable theme assets.
- **Safety**: names are portable single directory components; ZIP size, entry count, per-file size, traversal, and symlink boundaries are enforced. A theme selected by any user cannot be deleted.

### Public blog directory

- **Structure**: the unauthenticated server root lists every Vault with `IsPublicBlog` enabled as a semantic article linking to `/b/<vault-id>`; it never renders private files or article bodies.
- **Identity**: each entry uses the configured blog logo, name, and description with the Vault name as the title fallback. Empty state explains that no blogs have been published without referring to the legacy single-Vault system setting.
- **Layout**: an intrinsic card grid uses the console canvas, ledger borders, and cobalt controls; it reflows without horizontal overflow from 375px upward.

### Built-in reading tools

- **Structure**: both `default` and `papertrail` place article content in a horizontally centered, left-aligned reading column. A generated heading directory sits beside the column when space permits and collapses above it on narrow viewports without shifting the article off center.
- **Actions**: both templates expose light, dark, permitted copy, and back-to-top controls. Papertrail additionally keeps logo and blog name at the left of its sticky top bar, custom links plus return-to-blog-home at the right, and a blog home containing identity plus every accessible shared article.
- **States**: copy is rendered only when the share allows it and swaps to a short success label before restoring. An empty heading directory hides itself; back-to-top remains available in the reading toolbar.
- **Accessibility**: utility controls use native buttons/links, the directory is a labelled navigation landmark, copy status is announced through a polite live region, and focus remains visible in both color schemes.

### Repository identity link

- **Structure**: every server-rendered console and public page, including both built-in reading templates, ends with a compact text link to the canonical GitHub repository.
- **Layout**: the identifier remains in document flow and is centered across the footer row so it cannot cover article text or controls at desktop or mobile widths.
- **Accessibility**: native external-link semantics, readable text, and a visible theme-colored focus outline.

### Obsidian conflict diff

- **Structure**: `.oss-conflict-modal` contains an `.oss-diff-preview` with line-level `.oss-diff-row` entries, each composed of `.oss-diff-marker` and `.oss-diff-text`; `.oss-diff-empty` handles no-difference output.
- **States**: removed rows use a red `var(--text-error)` marker and transparent red overlay; added rows use a yellow `var(--text-warning)` marker and transparent yellow overlay; neutral context rows remain unfilled. Long unchanged ranges collapse to a centered, faint italic `.is-omitted` row while preserving two context lines around edits.
- **Layout**: the preview owns scrolling at a 400px maximum height, with an 8px inset and one-pixel `var(--background-modifier-border)` border. Rows use the host monospace font and a marker-plus-wrapping-text grid so whitespace is preserved without page-level horizontal overflow. Titles and diff text preserve path segments with semantic break opportunities before separators; narrow titles keep CJK phrases intact while retaining emergency wrapping for long unbroken content.

### Plugin localization

- **Modes**: `auto`, `zh`, and `en`. Auto resolves Obsidian's public UI language to Chinese only for `zh*`; every other locale resolves to English.
- **Coverage**: Ribbon tooltip, commands, status bar, settings, sidebar, notices, and modals all use one typed dictionary.
- **Consistency**: English resources contain no CJK text. Chinese resources translate labels and actions rather than mixing English UI terms into Chinese sentences; product names such as OSS and Obsidian remain proper nouns.
- **Switching**: changing the preference saves immediately and redraws settings/sidebar surfaces without restarting Obsidian.

## 6. Motion & Interaction

Navigation arrows use a 150ms state transition, switches use 160ms, copy label swaps use 150ms opacity feedback, and the mobile drawer uses 200ms. Motion is limited to transforms and opacity where possible. `prefers-reduced-motion: reduce` disables non-essential transitions and smooth scrolling.

## 7. Depth & Surface

The console uses a mixed border-and-shadow strategy. Strong one-pixel borders define operational regions; low, cool-tinted shadows elevate panels; offset ink shadows make primary and compact controls tactile. Dark mode preserves hierarchy with darker tonal surfaces and stronger black shadows.

## 8. Accessibility Constraints & Accepted Debt

The target is WCAG 2.2 AA: visible keyboard focus, native form semantics, status messaging through appropriate live roles, reduced-motion support, and readable reflow from 375px upward. Primary page content must not create document-level horizontal scrolling.

New work must not introduce additional accessibility debt or undeclared colors. Existing raw color literals in legacy console rules remain a consolidation opportunity.
