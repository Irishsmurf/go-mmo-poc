# Go MMO Proof-of-Concept

This project is a basic proof-of-concept demonstrating core principles for a massively multiplayer online (MMO) game server and client interaction using Go. It showcases:

*   Client-Server architecture over WebSockets.
*   State synchronization using Protocol Buffers (Protobuf).
*   Server-side spatial partitioning (Grid-based Area of Interest - AoI) for efficient updates.
*   World data chunking and on-demand loading.
*   Zlib compression for world chunk data.
*   A simple graphical client using Ebitengine for visualization and basic interaction.

**Disclaimer:** This is a *proof-of-concept*, not a complete or production-ready game. It lacks many features like robust error handling, sophisticated client-side prediction/interpolation, advanced physics, security, database persistence, etc.

## Features

*   **WebSocket Communication:** Real-time bidirectional communication using `gorilla/websocket`.
*   **Protocol Buffers:** Efficient binary serialization for network messages.
*   **Spatial Grid (Server):** Organizes players into grid cells for efficient Area of Interest (AoI) queries.
*   **Chunking & Compression:** World terrain data is divided into chunks, compressed with Zlib, and sent to clients on demand.
*   **Ebitengine Client:** A graphical client built with Ebitengine to visualize the world, other players, and allow basic movement.
*   **Basic Player Movement:** Client can send simple movement input (forward, turn) to the server.
*   **Concurrency:** Server utilizes Go routines and channels for handling multiple clients and game loop logic.
*   **Profiling Enabled:** Includes `net/http/pprof` for performance analysis.

## Prerequisites

1.  **Go:** Version 1.18 or later recommended. ([Download Go](https://go.dev/dl/))
2.  **Protocol Buffers Compiler (`protoc`):** Version 3.x or later. ([Installation Guide](https://grpc.io/docs/protoc-installation/))
3.  **Go Protobuf Plugin (`protoc-gen-go`):**
    ```bash
    go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
    ```
    Ensure your Go bin directory (e.g., `~/go/bin` or `%USERPROFILE%\go\bin`) is in your system's `PATH`.

4.  **Ebitengine Client Build Dependencies:**
    *   Ebitengine relies on Cgo and system graphics/audio libraries. You **must** install these development headers *before* building the client.
    *   **On Debian/Ubuntu/Mint etc.:**
        ```bash
        sudo apt update
        # Essential X11 dev libraries often needed for GLFW used by Ebitengine
        sudo apt install libxrandr-dev libxcursor-dev libxi-dev libxinerama-dev libxxf86vm-dev libgl1-mesa-dev libgles2-mesa-dev xorg-dev libasound2-dev # Added audio dev
        ```
    *   **On Fedora/CentOS/RHEL etc.:**
        ```bash
        sudo dnf install libXrandr-devel libXcursor-devel libXi-devel libXinerama-devel libXxf86vm-devel mesa-libGL-devel mesa-libGLES-devel libX11-devel alsa-lib-devel # Added audio dev
        ```
        *(Use `yum` on older CentOS/RHEL)*
    *   **On macOS:** Xcode Command Line Tools usually suffice (`xcode-select --install`). Audio/VideoToolbox frameworks are generally included.
    *   **On Windows:** Needs a C compiler like GCC (often available via MinGW/MSYS2). See Ebitengine's installation guide for details.
    *   **Other Linux:** Find equivalent development packages for the X11 and OpenGL/GLES libraries, and ALSA (or your audio system's dev package).

## Setup

1.  **Clone the Repository:**
    ```bash
    git clone <your-repo-url>
    cd go-mmo-poc
    ```

2.  **Update Module Path (If Necessary):**
    *   If you cloned from a different location than the module path defined in `go.mod` and `.go` files (e.g., `github.com/<your_username>/go-mmo-poc`), you may need to update the import paths in the `.go` files accordingly. A search/replace for the old path with your new module path might be needed.

3.  **Tidy Dependencies:**
    ```bash
    go mod tidy
    ```

## Building

1.  **Generate Protobuf Code:**
    *   Run this command from the project root directory (`go-mmo-poc/`):
        ```bash
        protoc --go_out=. --go_opt=paths=source_relative proto/mmo.proto
        ```
    *   This generates `proto/mmo.pb.go`.

2.  **Build the Server:**
    *   Run this command from the project root directory:
        ```bash
        go build -o mmo-server main.go
        ```
    *   This creates the `mmo-server` executable.

3.  **Build the Ebitengine Client:**
    *   **Ensure Ebitengine prerequisites are installed first!**
    *   Run this command from the project root directory:
        ```bash
        go build -o mmo-client-ebitengine client-ebitengine/main.go
        ```
    *   This creates the `mmo-client-ebitengine` executable.

## Running

1.  **Start the Server:**
    *   Open a terminal in the project root directory.
    *   Run the server executable:
        ```bash
        ./mmo-server
        ```
    *   By default, the server listens for WebSocket connections on port `8080` and serves pprof data on port `6060`.

2.  **Start the Client(s):**
    *   Open one or more *separate* terminals.
    *   Navigate to the project root directory.
    *   Run the client executable:
        ```bash
        ./mmo-client-ebitengine
        ```
    *   An Ebitengine window should appear, connect to the server, and display the game world.
    *   **Controls:**
        *   **Arrow Keys / WASD:** Move player (sends input to server).
        *   **Mouse Wheel:** Zoom camera in/out.
        *   **ESC:** Quit the client window.

## How It Works (Briefly)

*   The **Server** (`mmo-server`) manages the game state, including player positions and world data. It uses a spatial grid to efficiently determine which players are relevant to each other (Area of Interest). It listens for WebSocket connections.
*   The **Client** (`mmo-client-ebitengine`) connects to the server via WebSockets.
*   On connection, the server sends `InitData` (player ID, initial state, world info, color map).
*   The client requests nearby `WorldChunk` data based on its camera view. The server sends compressed chunks.
*   The server periodically sends `StateUpdate` messages containing the state of entities within the client's AoI.
*   The client decompresses chunks, renders the terrain and players using Ebitengine.
*   The client sends `PlayerInput` messages when movement keys are pressed.
*   All WebSocket communication uses serialized Protocol Buffers messages.

## Future Improvements / TODO

*   Client-side Interpolation for smooth remote player movement.
*   Client-side Prediction for responsive local player movement.
*   Delta Compression for `StateUpdate` messages (send only changed fields).
*   More sophisticated graphics (sprites instead of simple shapes).
*   Actual game mechanics (interaction, combat, etc.).
*   Robust error handling and reconnection logic.
*   Use UDP for frequent state updates (requires libraries like KCP or manual implementation).
*   Persistence (saving/loading world state and player data).
*   Scalability testing and further optimization (e.g., multi-process server).