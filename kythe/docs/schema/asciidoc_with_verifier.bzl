load("//tools:build_rules/doc.bzl", "asciidoc")

def asciidoc_with_verifier(name, src):
  asciidoc(
    name = name,
    src = src,
    confs = [
        "kythe-filter.conf",
    ],
    data = [
        "example.sh",
        "example-clike.sh",
        "example-cxx.sh",
        "example-objc.sh",
        "example-dot.sh",
        "example-java.sh",
        "java-schema-file-data-template.FileData",
        "java-schema-unit-template.CompilationUnit",
        "@io_bazel_rules_go_toolchain//:toolchain",
    ],
    tags = ["manual"],
    tools = [
        "//kythe/cxx/indexer/cxx:indexer",
        "//kythe/cxx/tools:kindex_tool",
        "//kythe/cxx/verifier",
        "//kythe/go/indexer/cmd/go_example:go_example",
        "//kythe/java/com/google/devtools/kythe/analyzers/java:indexer",
    ]
  )
