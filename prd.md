### **Product Requirements Document: Repowire**

---

### **1. Vision & Strategy**

**The Problem:** Modern engineering is rarely single-repo. A "Full Stack" feature often requires changes in a Backend Repo (API), a Frontend Repo (UI), and an Infrastructure Repo (Terraform). Current AI coding assistants (OpenCode, Claude Code) are single-process silos. They lack **lateral awareness**. The user is forced to act as the "Context Mule," manually copying file paths, code snippets, and requirements between isolated terminal windows.

**The Solution:** **Repowire** is a local "Service Mesh for Agents." It instantiates multiple OpenCode sessions (one per repository) and links them via a high-speed local bus. It injects a "Communication Protocol" into each agent, allowing them to autonomously coordinate, query each other, and synchronize state without human mediation.

**Core Philosophy:**
*   **Local First:** No cloud orchestration overhead. It runs on your machine.
*   **Peer-to-Peer:** Agents treat other agents as "Live Tools."
*   **Context-over-Content:** Agents share *pointers* (file paths, symbols) rather than raw text where possible, preserving context windows.

---

### **2. Architecture: The Local Agent Mesh**

Repowire functions as a central **Hypervisor** for OpenCode sessions.

#### **2.1. The System Layout**
* **Central Hub (Repowire Daemon):** Holds the "Global State" of the project and manages the message bus.
* **Nodes (OpenCode Sessions):** One isolated session per repository (e.g., `/frontend`, `/backend`).
* **The "Wire" (Inter-Agent Protocol):** A standard set of tools injected into every agent (e.g., `ask_peer`, `broadcast_update`) that allows them to communicate.

#### **2.2. Data Flow**
1.  **User Intent:** User types "Add login page" into the Repowire TUI.
2.  **Orchestration:** Repowire breaks this down:
    *   To Backend Agent: "Scaffold Auth API."
    *   To Frontend Agent: "Wait for Backend API spec, then build Login form."
3.  **Peer-to-Peer Sync:** Backend Agent finishes and calls `broadcast_update(artifact="openapi.json")`. Frontend Agent wakes up, consumes the artifact, and starts coding.

---

### **3. Key Features**

#### **3.1. Auto-Injected Connectivity Tools**
When Repowire spawns a session, it dynamically injects these special tools into the agent's context (via the OpenCode SDK):

*   **`ask_peer(target_repo, query)`**:
    *   *Usage:* Frontend agent asks Backend: "What is the return type of `GET /me`?"
    *   *Mechanism:* Repowire pauses the Frontend, prompts the Backend agent with the query, and returns the answer to the Frontend.
*   **`notify_peer(target_repo, message)`**:
    *   *Usage:* "I have finished the database migration."
    *   *Mechanism:* Sends a system notification to the target agent's event stream.
*   **`read_peer_file(target_repo, file_path)`**:
    *   *Usage:* Frontend agent needs to see the raw `user.go` model struct to define TypeScript interfaces.
    *   *Mechanism:* Securely reads the file from the other repo's directory without hallucinating or requiring copy-paste.

#### **3.2. The "Blackboard" (Shared State)**
A persistent JSON object shared across all agents, useful for global constants or status tracking.
*   **`write_blackboard(key, value)`**: Agent sets `API_ENDPOINT = "localhost:8080"`.
*   **`read_blackboard(key)`**: Agent reads the value.
*   **Events:** Updating a key sends a "Context Update" event to all subscribed agents.

#### **3.3. The "War Room" TUI**
A unified Terminal UI that shows:
*   **Split Pane View:** See `frontend` and `backend` agent logs side-by-side.
*   **Global Chat:** A master input box that sends instructions to the Orchestrator.
*   **Dependency Graph:** A live visualization of cross-repo blockers (e.g., "Frontend waiting for Backend PR #102").

---

### **4. Configuration & Bootstrapping**

Repowire uses a simple YAML config in the root of your workspace to define the mesh.

**`repowire.yaml`**:
```yaml
name: "E-commerce Stack"
agents:
  backend:
    path: "./backend-api"
    model: "claude-3-5-sonnet"
    color: "blue"
    capabilities: ["db_access", "migrations"]
  frontend:
    path: "./web-client"
    model: "claude-3-5-sonnet"
    color: "green"
    depends_on: ["backend"] # Suggests build order
  infra:
    path: "./terraform"
    model: "gpt-4o"
    color: "yellow"
```

*   **Startup:** `repowire up` reads this config, spawns the sessions, and opens the TUI.
*   **Git Awareness:** Repowire warns if repos are on divergent branches (e.g., Backend is on `feature/auth` but Frontend is on `main`).

---

### **5. User Stories**

*   **The "API Contract" Negotiation:**
    *   *User:* "Refactor the User API to include 'middle_name'."
    *   *Backend Agent:* Updates the Python model and Pydantic schema. Calls `notify_peer("frontend", "Updated User schema")` and `write_blackboard("USER_SCHEMA_HASH", "abc1234")`.
    *   *Frontend Agent:* Receives notification. Automatically runs `npm run codegen`. Detects a breaking change in `UserCard.tsx`. Fixes it.
    *   *Result:* Zero broken builds, zero manual coordination.

*   **The "Integration Test" Debugger:**
    *   *User:* "Why is the E2E test failing?"
    *   *Test Agent:* "It fails on step 3: Login." Calls `ask_peer("backend", "Check logs for request ID 123")`.
    *   *Backend Agent:* "Found 500 error: Database connection timeout."
    *   *Test Agent:* Reports back to user: "Test failed due to DB timeout in backend."

---

### **6. Technical Implementation**

Built on `opencode-sdk-python` and `asyncio`.

```python
# Conceptual Architecture
class RepowireOrchestrator:
    def __init__(self, config_path="repowire.yaml"):
        self.config = load_config(config_path)
        self.nodes = {} 
        self.blackboard = {}

    async def boot_mesh(self):
        # Spawn all agents defined in yaml
        for name, cfg in self.config["agents"].items():
            await self.spawn_agent(name, cfg["path"])

    async def spawn_agent(self, name, repo_path):
        client = AsyncOpencode(cwd=repo_path)
        session = await client.session.create()
        
        # Inject "The Wire" Tools
        # Note: Actual tool injection depends on SDK capability to add tools to active sessions
        # or defining them at session start.
        await self.inject_tools(client, session.id, name)
        
        self.nodes[name] = {"client": client, "session": session}

    async def handle_blackboard_write(self, key, value, source_agent):
        self.blackboard[key] = value
        # Broadcast update to others
        await self.broadcast_event(f"CONTEXT_UPDATE: {key} changed by {source_agent}")

    async def handle_peer_query(self, source, target, query):
        # Route question to target agent
        target_node = self.nodes.get(target)
        response = await target_node["client"].session.send_message(
            f"[SYSTEM] Agent '{source}' needs info: {query}"
        )
        return response.text
```

---

### **7. References & Inspiration**

This project draws architectural inspiration from several key areas in modern software engineering and agentic workflows:

*   **[Service Mesh (Istio)](https://istio.io/) / [Linkerd](https://linkerd.io/):** Just as a service mesh manages traffic, observability, and security between microservices, **Repowire** manages context, instructions, and tools between "Micro-Agents."
*   **[Blackboard Pattern](https://en.wikipedia.org/wiki/Blackboard_system):** A classic AI architecture where multiple specialized knowledge sources (agents) write to a shared memory space (the blackboard) to solve a common problem collaboratively.
*   **[LangGraph](https://langchain-ai.github.io/langgraph/) / [AutoGen](https://microsoft.github.io/autogen/):** Influential multi-agent frameworks that pioneered the concept of "Agent Teams." Repowire adapts these concepts specifically for the **Localhost + Multi-Repo** use case, rather than cloud-based chat apps.
*   **[A2A (Agent-to-Agent) Protocol](https://github.com/transitive-bullshit/agent-to-agent):** The emerging standard for interoperable agent communication. Repowire aims to be a reference implementation of A2A for local development environments, proving that agents can negotiate contracts and tools without a central cloud broker.
*   **[Docker Compose](https://docs.docker.com/compose/):** The `repowire.yaml` configuration is deliberately modeled after `docker-compose.yml`. It defines a "stack" of agents, their build contexts (repos), and their dependencies, making it familiar to any DevOps engineer.
*   **[Oh My OpenCode](https://github.com/code-yeongyu/oh-my-opencode):** Demonstrates the power of "Battery-Included" agent configurations and sub-agent delegation within the OpenCode ecosystem.

---

### **8. Why This Matters**
**Repowire** solves the "Siloed Intelligence" problem. By treating agents as a **collaborative distributed system** rather than isolated chat windows, we unlock the true potential of AI for complex, real-world software engineering that spans multiple domains and repositories.
