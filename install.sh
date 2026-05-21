#!/bin/sh
# Repowire installer — curl -sSf https://repowire.io/install | sh
set -e

echo "Installing repowire..."
echo ""

# Check Python >= 3.10
python_cmd=""
for cmd in python3 python; do
    if command -v "$cmd" >/dev/null 2>&1; then
        version=$("$cmd" -c "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')")
        major=$(echo "$version" | cut -d. -f1)
        minor=$(echo "$version" | cut -d. -f2)
        if [ "$major" -ge 3 ] && [ "$minor" -ge 10 ]; then
            python_cmd="$cmd"
            break
        fi
    fi
done

if [ -z "$python_cmd" ]; then
    echo "Error: Python 3.10+ is required."
    echo "Install Python from https://python.org or via your package manager."
    exit 1
fi
echo "Found $python_cmd ($version)"

# Check tmux
if ! command -v tmux >/dev/null 2>&1; then
    echo "Warning: tmux not found (needed for peer spawning)."
    echo "  macOS: brew install tmux"
    echo "  Linux: apt install tmux / yum install tmux"
fi

# Install via uv > pipx > pip
if command -v uv >/dev/null 2>&1; then
    if command -v repowire >/dev/null 2>&1; then
        echo "Upgrading via uv..."
        uv tool upgrade repowire || uv tool install repowire --force
    else
        echo "Installing via uv..."
        uv tool install repowire
    fi
elif command -v pipx >/dev/null 2>&1; then
    if command -v repowire >/dev/null 2>&1; then
        echo "Upgrading via pipx..."
        pipx upgrade repowire || pipx install --force repowire
    else
        echo "Installing via pipx..."
        pipx install repowire
    fi
elif "$python_cmd" -m pip --version >/dev/null 2>&1; then
    echo "Installing/upgrading via pip..."
    "$python_cmd" -m pip install --user -U repowire
    echo ""
    echo "Note: ensure ~/.local/bin is in your PATH"
else
    echo "Error: No package manager found (uv, pipx, or pip)."
    echo "Install uv: curl -LsSf https://astral.sh/uv/install.sh | sh"
    exit 1
fi

echo ""

# Verify installation
if command -v repowire >/dev/null 2>&1; then
    echo "repowire $(repowire --version) installed."
    echo ""
    echo "Running setup..."
    echo ""
    repowire setup "$@"
else
    echo "repowire installed but not on PATH."
    echo "Add ~/.local/bin to your PATH, then run: repowire setup"
fi
