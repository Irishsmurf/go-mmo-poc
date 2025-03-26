# WORKSPACE (or WORKSPACE.bazel)

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")
load("@bazel_tools//tools/build_defs/repo:git.bzl", "git_repository")

# --- Go Rules ---
# Provides rules for building Go code (go_library, go_binary, go_test)
http_archive(
    name = "io_bazel_rules_go",
    urls = [
        "https://mirror.bazel.build/github.com/bazelbuild/rules_go/releases/download/v0.47.1/rules_go-v0.47.1.zip",
        "https://github.com/bazelbuild/rules_go/releases/download/v0.47.1/rules_go-v0.47.1.zip",
    ],
)

# Load Go dependencies using Gazelle (recommended) or manually declare them
# Gazelle automates Go dependency management within Bazel
http_archive(
    name = "bazel_gazelle",
    urls = [
        "https://mirror.bazel.build/github.com/bazelbuild/bazel-gazelle/releases/download/v0.36.0/bazel-gazelle-v0.36.0.tar.gz",
        "https://github.com/bazelbuild/bazel-gazelle/releases/download/v0.36.0/bazel-gazelle-v0.36.0.tar.gz",
    ],
)

# Load rules_go and Gazelle dependencies
load("@io_bazel_rules_go//go:deps.bzl", "go_register_toolchains", "go_rules_dependencies")
load("@bazel_gazelle//:deps.bzl", "gazelle_dependencies")

go_rules_dependencies()
go_register_toolchains(version = "1.21.0") # Specify your desired Go SDK version

gazelle_dependencies()


# --- Protobuf Rules ---
http_archive(
    name = "rules_proto",
    strip_prefix = "rules_proto-6.0.0-rc1",
    urls = ["https://github.com/bazelbuild/rules_proto/archive/refs/tags/6.0.0-rc1.zip"],
)
load("@rules_proto//proto:repositories.bzl", "rules_proto_dependencies", "rules_proto_toolchains")
rules_proto_dependencies()
rules_proto_toolchains()

# --- Ebitengine Dependencies (GLFW & System Libraries) ---
# Bazel needs to know about the system libraries Ebitengine/GLFW requires.
# This often involves defining cc_library targets pointing to system headers/libs,
# which can be complex and platform-specific.
# A simpler approach for now is to rely on the system having the necessary
# -dev/-devel packages installed (as done previously) and letting Cgo find them.
# We will add necessary Cgo flags in the BUILD files later.

# NOTE: For fully hermetic C/C++ dependencies, you would use rules like rules_cc
# and potentially define toolchains or use pre-built libraries within the workspace.
# This adds significant complexity.