---
name: textual
description: "Expert guidance for building TUI (Text User Interface) applications with the Textual Python framework. Use when: (1) Building or modifying terminal user interfaces, (2) Working with Textual widgets, screens, or layouts, (3) Implementing CSS styling (TCSS) for TUIs, (4) Setting up reactive programming patterns, (5) Testing Textual applications with pytest/Pilot, (6) Debugging Textual code or encountering Textual errors, (7) Questions about TUI design patterns or architecture."
---

# Textual - Python TUI Framework

## Core Architecture

Textual apps are **event-driven** with an async message queue. Key components:

- **App** - Entry point, manages screens and global state
- **Screens** - Full-terminal containers for widgets
- **Widgets** - Reusable UI components with compose/render methods
- **Messages** - Communication between components (bubble up the DOM)
- **CSS (TCSS)** - Styling separate from logic

## Essential Patterns

### Basic App

```python
from textual.app import App, ComposeResult
from textual.widgets import Header, Footer, Static

class MyApp(App):
    CSS_PATH = "app.tcss"

    def compose(self) -> ComposeResult:
        yield Header()
        yield Static("Hello, Textual!")
        yield Footer()

if __name__ == "__main__":
    MyApp().run()
```

### Reactive Attributes

```python
from textual.reactive import reactive

class Counter(Widget):
    count = reactive(0)  # Auto-refreshes UI on change

    def render(self) -> str:
        return f"Count: {self.count}"

    def validate_count(self, value: int) -> int:
        return max(0, value)  # Constrain values

    def watch_count(self, old: int, new: int) -> None:
        if new > 10:
            self.add_class("high")
```

### Widget Communication

**"Attributes down, messages up"** - Parent sets child attributes, children post messages to parents:

```python
class ChildWidget(Widget):
    class Updated(Message):
        def __init__(self, value: int) -> None:
            super().__init__()
            self.value = value

    def update_value(self) -> None:
        self.post_message(self.Updated(self.value))

class ParentWidget(Widget):
    def on_child_widget_updated(self, message: ChildWidget.Updated) -> None:
        self.log(f"Child updated: {message.value}")
```

### Testing

```python
@pytest.mark.asyncio
async def test_button_click():
    app = MyApp()
    async with app.run_test() as pilot:
        await pilot.click("#submit-button")
        await pilot.pause()  # CRITICAL: Wait for message processing
        assert app.query_one("#status").renderable == "Success"
```

## Common Mistakes

1. **Forgetting async/await** - `mount()`, `push_screen()` are async
2. **Missing `pilot.pause()` in tests** - Race condition without it
3. **Modifying reactives in `__init__`** - Use `set_reactive()` or `on_mount()`
4. **Blocking event loop** - Use `@work` decorator for async operations

## Development Commands

```bash
textual run --dev my_app.py    # Live CSS reload
textual console                 # Debug console (run in separate terminal)
```

## References

- **[quick-reference.md](references/quick-reference.md)** - Templates, CSS cheat sheet, testing patterns
- **[guide.md](references/guide.md)** - Full architecture, design principles, state management
