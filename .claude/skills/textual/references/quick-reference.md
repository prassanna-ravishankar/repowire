# Textual Quick Reference

## Table of Contents

1. [App Templates](#app-templates)
2. [Widget Lifecycle](#widget-lifecycle)
3. [CSS Cheat Sheet](#css-cheat-sheet)
4. [Common Patterns](#common-patterns)
5. [Testing Templates](#testing-templates)
6. [Built-in Widgets](#built-in-widgets)

---

## App Templates

### Multi-Screen App

```python
from textual.app import App, ComposeResult
from textual.screen import Screen
from textual.widgets import Header, Footer, Button

class MainScreen(Screen):
    def compose(self) -> ComposeResult:
        yield Header()
        yield Button("Go to Settings", id="settings-btn")
        yield Footer()

    def on_button_pressed(self) -> None:
        self.app.push_screen("settings")

class SettingsScreen(Screen):
    def compose(self) -> ComposeResult:
        yield Header()
        yield Button("Back", id="back-btn")
        yield Footer()

    def on_button_pressed(self) -> None:
        self.app.pop_screen()

class MyApp(App):
    SCREENS = {"main": MainScreen, "settings": SettingsScreen}

    def on_mount(self) -> None:
        self.push_screen("main")
```

### Modal Dialog

```python
from textual.screen import ModalScreen
from textual.containers import Vertical, Horizontal

class ConfirmDialog(ModalScreen[bool]):
    def compose(self) -> ComposeResult:
        with Vertical(id="dialog"):
            yield Static("Are you sure?")
            with Horizontal():
                yield Button("Yes", id="yes", variant="primary")
                yield Button("No", id="no")

    def on_button_pressed(self, event: Button.Pressed) -> None:
        self.dismiss(event.button.id == "yes")

# Usage
def action_delete(self) -> None:
    def handle_result(confirmed: bool) -> None:
        if confirmed:
            self.delete_item()
    self.push_screen(ConfirmDialog(), handle_result)
```

---

## Widget Lifecycle

```python
class MyWidget(Widget):
    def __init__(self, **kwargs) -> None:
        """Created - don't modify reactive attributes here."""
        super().__init__(**kwargs)

    def compose(self) -> ComposeResult:
        """Build child widgets."""
        yield ChildWidget()

    def on_mount(self) -> None:
        """Mounted to DOM - safe to modify reactives."""
        self.set_interval(1, self.update_data)

    def on_unmount(self) -> None:
        """Before removal - cleanup resources."""
        pass

    def on_show(self) -> None:
        """Widget becomes visible."""
        pass

    def on_hide(self) -> None:
        """Widget becomes hidden."""
        pass
```

---

## CSS Cheat Sheet

### Layout

```css
/* Docking */
#header { dock: top; height: 3; }
#sidebar { dock: left; width: 30; }
#footer { dock: bottom; height: 1; }

/* Flexible sizing */
#content { width: 1fr; height: 1fr; }
#left { width: 1fr; }
#right { width: 2fr; }  /* Twice as wide */

/* Grid */
#container {
    layout: grid;
    grid-size: 3 2;
    grid-columns: 1fr 2fr 1fr;
    grid-gutter: 1 2;
}

/* Alignment */
Screen { align: center middle; }
#widget { text-align: center; content-align: center middle; }
```

### Visual

```css
/* Theme colors */
Button {
    background: $primary;
    color: $text;
    border: solid $accent;
}

/* Spacing */
.container { padding: 1 2; margin: 1; }

/* States */
Button:hover { background: $primary-lighten-1; }
Button:focus { border: solid yellow; }
Input:disabled { opacity: 0.5; }

/* Nesting */
Button {
    background: blue;
    &:hover { background: lightblue; }
    &.danger { background: red; }
}
```

### Theme Colors

```css
$primary, $secondary, $accent, $warning, $error, $success
$background, $surface, $panel, $boost
$text, $text-muted, $text-disabled
$primary-lighten-1/2/3, $primary-darken-1/2/3, $primary-muted
```

---

## Common Patterns

### Actions and Key Bindings

```python
class MyApp(App):
    BINDINGS = [
        ("ctrl+s", "save", "Save"),
        ("ctrl+q", "quit", "Quit"),
    ]

    def action_save(self) -> None:
        self.save_data()
```

### Workers for Async

```python
from textual.worker import work

class MyWidget(Widget):
    @work(exclusive=True)
    async def load_data(self) -> None:
        data = await fetch_from_api()
        self.display_data(data)

    def on_button_pressed(self) -> None:
        self.load_data()  # Don't await
```

### Data Binding

```python
class ParentWidget(Widget):
    value = reactive(0)

    def compose(self) -> ComposeResult:
        child = ChildWidget()
        child.data_bind(self, display="value")
        yield child
```

### Query Widgets

```python
button = self.query_one("#submit", Button)  # By ID
header = self.query_one(Header)              # By type
buttons = self.query("Button")               # Multiple
first = self.query("Button").first()
enabled = self.query("Button").filter(".enabled")
```

### Containers

```python
from textual.containers import Vertical, Horizontal, Grid, Center

def compose(self) -> ComposeResult:
    with Vertical():
        yield Widget1()
        yield Widget2()
    with Horizontal():
        yield Widget3()
        yield Widget4()
```

---

## Testing Templates

### Keyboard Input

```python
@pytest.mark.asyncio
async def test_keyboard():
    app = MyApp()
    async with app.run_test() as pilot:
        await pilot.press("tab")
        await pilot.pause()
        assert app.focused.id == "first-input"

        await pilot.press("h", "e", "l", "l", "o")
        await pilot.pause()
        assert app.query_one("#first-input").value == "hello"
```

### Form Submission

```python
@pytest.mark.asyncio
async def test_form():
    app = MyApp()
    async with app.run_test() as pilot:
        app.query_one("#name").value = "Alice"
        app.query_one("#email").value = "alice@example.com"
        await pilot.click("#submit")
        await pilot.pause()
        assert app.user_data["name"] == "Alice"
```

### Screen Navigation

```python
@pytest.mark.asyncio
async def test_navigation():
    app = MyApp()
    async with app.run_test() as pilot:
        assert app.screen.name == "main"
        app.push_screen("settings")
        await pilot.pause()
        assert app.screen.name == "settings"
```

### Custom Size

```python
async with app.run_test(size=(40, 20)) as pilot:
    assert app.query_one("#sidebar").has_class("compact")
```

---

## Built-in Widgets

### Input & Selection
`Button`, `Checkbox`, `Input`, `MaskedInput`, `RadioButton`, `RadioSet`, `Select`, `SelectionList`, `Switch`, `TextArea`

### Display
`Label`, `Static`, `Pretty`, `Digits`, `Markdown`, `MarkdownViewer`, `Rule`

### Data
`DataTable`, `ListView`, `OptionList`, `Tree`, `DirectoryTree`

### Containers
`Header`, `Footer`, `Tabs`, `TabbedContent`, `Collapsible`, `ContentSwitcher`, `Vertical`, `Horizontal`, `Grid`, `Center`, `Container`

### Feedback
`ProgressBar`, `LoadingIndicator`, `Placeholder`, `Log`, `RichLog`
