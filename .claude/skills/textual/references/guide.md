# Textual Framework Guide

## Table of Contents

1. [Core Architecture](#core-architecture)
2. [CSS Styling System](#css-styling-system)
3. [Reactive System](#reactive-system)
4. [Project Structure](#project-structure)
5. [Design Best Practices](#design-best-practices)
6. [Common Errors](#common-errors)
7. [Debugging](#debugging)
8. [State Management](#state-management)

---

## Core Architecture

### Event-Driven System

When `app.run()` is called, Textual enters "application mode," taking control of the terminal. It uses an **async message queue** where events are processed sequentially. Each App and Widget has its own message queue.

### The DOM

Textual implements a DOM-like structure where widgets form a tree hierarchy. Query widgets using CSS selectors:

```python
query_one("#id")           # Single widget by ID
query_one(Button)          # Single widget by type
query("Button")            # All matching widgets
query("Button").first()    # First match
query("Button").filter(".enabled")  # Filtered
```

### Event Bubbling

Events bubble up the DOM hierarchy by default. Call `event.stop()` to halt propagation.

### Screens

- Only one screen active at a time
- Support push/pop navigation stack
- Can be modal for dialogs
- Define their own key bindings and CSS

```python
self.push_screen("settings")  # Push onto stack
self.pop_screen()             # Pop topmost
self.switch_screen("help")    # Replace top
self.dismiss(result_data)     # Pop and return data
```

### Widget Features

```python
class CustomWidget(Widget):
    DEFAULT_CSS = """
    CustomWidget { border: solid blue; padding: 1; }
    """

    can_focus = True  # Make focusable

    BINDINGS = [
        ("enter", "select", "Select item"),
    ]

    def __init__(self) -> None:
        super().__init__()
        self.border_title = "My Widget"
        self.tooltip = "Helpful tooltip"
```

---

## CSS Styling System

CSS separates appearance from logic. Use `CSS_PATH` for external files (enables live reload) or `CSS` for inline.

### Selectors

```css
Button { }              /* Type */
#next { }               /* ID */
.success { }            /* Class */
* { }                   /* Universal */
#dialog Button { }      /* Descendant */
#sidebar > Button { }   /* Direct child */
```

### Pseudo-classes

```css
Button:hover { }
Button:focus { }
.form-input:disabled { }
Widget:dark { }   /* Dark theme */
Widget:light { }  /* Light theme */
```

### Variables and Nesting

```css
Screen { background: $surface; }

Button {
    background: $primary;
    &:hover { background: $primary-lighten-1; }
    &.danger { background: $error; }
}
```

---

## Reactive System

### Features

**Auto-refresh**: UI updates automatically when reactive values change.

**Validation**: Constrain values before assignment.

```python
def validate_count(self, value: int) -> int:
    return max(0, min(value, 100))
```

**Watchers**: React to changes.

```python
def watch_count(self, old: int, new: int) -> None:
    if new > 10:
        self.add_class("high")
```

**Computed properties**: Auto-recalculate derived values.

```python
doubled = reactive(0)

def compute_doubled(self) -> int:
    return self.count * 2
```

**Recompose**: Rebuild widget tree when data changes.

```python
mode = reactive("list", recompose=True)

def compose(self) -> ComposeResult:
    if self.mode == "list":
        yield ListView()
    else:
        yield GridView()
```

---

## Project Structure

### Medium/Large Apps

```
project/
├── src/
│   ├── app.py
│   ├── screens/
│   │   ├── main_screen.py
│   │   └── settings_screen.py
│   ├── widgets/
│   │   ├── status_bar.py
│   │   └── data_grid.py
│   └── business_logic/
│       ├── models.py
│       └── services.py
├── static/
│   └── app.tcss
├── tests/
│   ├── test_app.py
│   └── test_widgets/
└── pyproject.toml
```

### Organization Principles

1. **Widget Communication**: "Attributes down, messages up"
2. **CSS for Widgets**: Use `DEFAULT_CSS` for distributable widgets
3. **CSS for Apps**: External files for live editing
4. **Compound Widgets**: Build complex from simple via composition

---

## Design Best Practices

### UI/UX

1. **Sketch first** - Draw rectangles on paper before coding
2. **Work outside-in** - Fixed elements first (header/footer), then flexible content
3. **Use docking** - Fix positions with `dock: top/bottom/left/right`
4. **FR units** - `1fr` for flexible sizing

### Code Organization

- **Prefer composition** over inheritance
- **Single responsibility** - Each widget handles one thing
- **Separate concerns** - UI in widgets, logic in services
- **Type hints** everywhere

### Performance

1. Target 60fps
2. Use `Static` widget for cached rendering
3. Cache expensive operations with `@lru_cache`
4. Use immutable data structures
5. Workers for async operations

---

## Common Errors

### 1. Forgetting async/await

```python
# WRONG
def on_button_pressed(self):
    self.mount(Widget())

# RIGHT
async def on_button_pressed(self):
    await self.mount(Widget())
```

### 2. Missing pilot.pause() in tests

```python
# WRONG - race condition
await pilot.click("#button")
assert app.query_one("#status").text == "Done"

# RIGHT
await pilot.click("#button")
await pilot.pause()
assert app.query_one("#status").text == "Done"
```

### 3. Modifying reactives in __init__

```python
# WRONG - triggers watchers too early
def __init__(self):
    super().__init__()
    self.count = 10

# RIGHT
def __init__(self):
    super().__init__()
    self.set_reactive(MyWidget.count, 10)
```

### 4. Blocking the event loop

```python
# WRONG
def on_button_pressed(self):
    response = requests.get(url)  # Blocks!

# RIGHT
@work(exclusive=True)
async def on_button_pressed(self):
    response = await httpx.get(url)
```

---

## Debugging

### Development Console

Terminal 1:
```bash
textual console
```

Terminal 2:
```bash
textual run --dev my_app.py
```

### In-App Logging

```python
from textual import log

def on_button_pressed(self):
    log("Button pressed!")
    log("State:", self.state)
    log(locals())
```

### Visual Debugging

```css
* { border: solid red; }  /* See layout structure */
```

---

## State Management

### Local Widget State

```python
class Counter(Widget):
    count = reactive(0)

    def increment(self) -> None:
        self.count += 1
```

### App-Level State

```python
class MyApp(App):
    user_name = reactive("")
    is_authenticated = reactive(False)

# Any widget accesses via self.app
class UserWidget(Widget):
    def render(self) -> str:
        if self.app.is_authenticated:
            return f"Welcome, {self.app.user_name}!"
        return "Please log in"
```

### Message-Based State

```python
class DataUpdated(Message):
    def __init__(self, data: dict) -> None:
        super().__init__()
        self.data = data

class DataWidget(Widget):
    def update_data(self, data: dict) -> None:
        self.data = data
        self.post_message(DataUpdated(data))

class ListenerWidget(Widget):
    def on_data_updated(self, message: DataUpdated) -> None:
        self.refresh_display(message.data)
```
