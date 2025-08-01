load("//tools:defs.bzl", "cc_library", "cc_test", "coreutil", "default_net_util", "gbenchmark_internal", "select_gtest", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    default_visibility = ["//:sandbox"],
    licenses = ["notice"],
)

cc_library(
    name = "capability_util",
    testonly = 1,
    srcs = [
        "fuchsia_capability_util.cc",
        "linux_capability_util.cc",
    ],
    hdrs = [
        "capability_util.h",
        "linux_capability_util.h",
    ],
    deps = [
        ":cleanup",
        ":memory_util",
        ":posix_error",
        ":save_util",
        ":socket_util",
        ":test_util",
        "@com_google_absl//absl/strings",
    ],
)

cc_library(
    name = "eventfd_util",
    testonly = 1,
    hdrs = ["eventfd_util.h"],
    deps = [
        ":file_descriptor",
        ":posix_error",
        ":save_util",
    ],
)

cc_library(
    name = "file_descriptor",
    testonly = 1,
    hdrs = ["file_descriptor.h"],
    deps = select_gtest() + [
        ":logging",
        ":posix_error",
        ":save_util",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/strings:str_format",
    ],
)

cc_library(
    name = "fuse_util",
    testonly = 1,
    srcs = ["fuse_util.cc"],
    hdrs = ["fuse_util.h"],
)

cc_library(
    name = "io_uring_util",
    testonly = 1,
    srcs = ["io_uring_util.cc"],
    hdrs = ["io_uring_util.h"],
    deps = [
        ":file_descriptor",
        ":posix_error",
        ":save_util",
        "//test/util:temp_path",
        "//test/util:test_util",
    ],
)

cc_library(
    name = "proc_util",
    testonly = 1,
    srcs = ["proc_util.cc"],
    hdrs = ["proc_util.h"],
    deps = select_gtest() + [
        ":fs_util",
        ":posix_error",
        ":test_util",
        "@com_google_absl//absl/algorithm:container",
        "@com_google_absl//absl/container:flat_hash_set",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/strings:str_format",
        "@com_google_absl//absl/types:optional",
    ],
)

cc_test(
    name = "proc_util_test",
    size = "small",
    srcs = ["proc_util_test.cc"],
    deps = select_gtest() + [
        ":proc_util",
        ":test_main",
        ":test_util",
    ],
)

cc_library(
    name = "cleanup",
    testonly = 1,
    hdrs = ["cleanup.h"],
)

cc_library(
    name = "fs_util",
    testonly = 1,
    srcs = ["fs_util.cc"],
    hdrs = ["fs_util.h"],
    deps = select_gtest() + [
        ":cleanup",
        ":file_descriptor",
        ":posix_error",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/time",
    ],
)

cc_test(
    name = "fs_util_test",
    size = "small",
    srcs = ["fs_util_test.cc"],
    deps = select_gtest() + [
        ":fs_util",
        ":posix_error",
        ":temp_path",
        ":test_main",
        ":test_util",
    ],
)

cc_library(
    name = "logging",
    testonly = 1,
    srcs = ["logging.cc"],
    hdrs = ["logging.h"],
)

cc_library(
    name = "memory_util",
    testonly = 1,
    hdrs = ["memory_util.h"],
    deps = [
        ":logging",
        ":posix_error",
        ":save_util",
        ":test_util",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/strings:str_format",
    ],
)

cc_library(
    name = "mount_util",
    testonly = 1,
    srcs = ["mount_util.cc"],
    hdrs = ["mount_util.h"],
    deps = select_gtest() + [
        ":cleanup",
        ":posix_error",
        ":temp_path",
        ":test_util",
        "@com_google_absl//absl/container:flat_hash_map",
        "@com_google_absl//absl/status:statusor",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/types:span",
    ],
)

cc_test(
    name = "mount_util_test",
    size = "small",
    srcs = ["mount_util_test.cc"],
    deps = select_gtest() + [
        ":mount_util",
        ":test_main",
        ":test_util",
    ],
)

cc_library(
    name = "save_util",
    testonly = 1,
    srcs = [
        "save_util.cc",
        "save_util_linux.cc",
        "save_util_other.cc",
    ],
    hdrs = ["save_util.h"],
    defines = select_system(),
    deps = [
        ":logging",
        "@com_google_absl//absl/types:optional",
    ],
)

cc_library(
    name = "multiprocess_util",
    testonly = 1,
    srcs = ["multiprocess_util.cc"],
    hdrs = ["multiprocess_util.h"],
    deps = select_gtest() + [
        ":cleanup",
        ":file_descriptor",
        ":logging",
        ":posix_error",
        ":save_util",
        ":test_util",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/strings:str_format",
    ],
)

cc_library(
    name = "platform_util",
    testonly = 1,
    srcs = ["platform_util.cc"],
    hdrs = ["platform_util.h"],
    deps = [":test_util"],
)

cc_library(
    name = "posix_error",
    testonly = 1,
    srcs = ["posix_error.cc"],
    hdrs = ["posix_error.h"],
    deps = select_gtest() + [
        ":logging",
        "@com_google_absl//absl/base:core_headers",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/types:variant",
    ],
)

cc_test(
    name = "posix_error_test",
    size = "small",
    srcs = ["posix_error_test.cc"],
    deps = select_gtest() + [
        ":posix_error",
        ":test_main",
    ],
)

cc_library(
    name = "pty_util",
    testonly = 1,
    srcs = ["pty_util.cc"],
    hdrs = ["pty_util.h"],
    deps = [
        ":file_descriptor",
        ":posix_error",
    ],
)

cc_library(
    name = "signal_util",
    testonly = 1,
    srcs = ["signal_util.cc"],
    hdrs = ["signal_util.h"],
    deps = select_gtest() + [
        ":cleanup",
        ":file_descriptor",
        ":posix_error",
        ":test_util",
    ],
)

cc_library(
    name = "temp_path",
    testonly = 1,
    srcs = ["temp_path.cc"],
    hdrs = ["temp_path.h"],
    deps = select_gtest() + [
        ":fs_util",
        ":posix_error",
        ":test_util",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/time",
    ],
)

cc_library(
    name = "test_util",
    testonly = 1,
    srcs = [
        "test_util.cc",
        "test_util_impl.cc",
        "test_util_runfiles.cc",
    ],
    hdrs = ["test_util.h"],
    defines = select_system(),
    deps = coreutil() + select_gtest() + [
        ":fs_util",
        ":logging",
        ":posix_error",
        ":save_util",
        "@bazel_tools//tools/cpp/runfiles",
        "@com_google_absl//absl/base:core_headers",
        "@com_google_absl//absl/flags:flag",
        "@com_google_absl//absl/flags:parse",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/strings:str_format",
        "@com_google_absl//absl/time",
        gbenchmark_internal,
    ],
)

cc_library(
    name = "thread_util",
    testonly = 1,
    hdrs = ["thread_util.h"],
    deps = [":logging"],
)

cc_library(
    name = "time_util",
    testonly = 1,
    srcs = ["time_util.cc"],
    hdrs = ["time_util.h"],
    deps = [
        "@com_google_absl//absl/time",
    ],
)

cc_library(
    name = "timer_util",
    testonly = 1,
    srcs = ["timer_util.cc"],
    hdrs = ["timer_util.h"],
    deps = select_gtest() + [
        ":cleanup",
        ":logging",
        ":posix_error",
        ":test_util",
        "@com_google_absl//absl/time",
    ],
)

cc_test(
    name = "test_util_test",
    size = "small",
    srcs = ["test_util_test.cc"],
    deps = select_gtest() + [
        ":test_main",
        ":test_util",
    ],
)

cc_library(
    name = "test_main",
    testonly = 1,
    srcs = ["test_main.cc"],
    deps = [":test_util"],
)

cc_library(
    name = "epoll_util",
    testonly = 1,
    srcs = ["epoll_util.cc"],
    hdrs = ["epoll_util.h"],
    deps = select_gtest() + [
        ":file_descriptor",
        ":posix_error",
        ":save_util",
    ],
)

cc_library(
    name = "rlimit_util",
    testonly = 1,
    srcs = ["rlimit_util.cc"],
    hdrs = ["rlimit_util.h"],
    deps = [
        ":cleanup",
        ":logging",
        ":posix_error",
        ":test_util",
    ],
)

cc_library(
    name = "uid_util",
    testonly = 1,
    srcs = ["uid_util.cc"],
    hdrs = ["uid_util.h"],
    deps = [
        ":posix_error",
        ":save_util",
    ],
)

cc_library(
    name = "temp_umask",
    testonly = 1,
    hdrs = ["temp_umask.h"],
)

cc_library(
    name = "cgroup_util",
    testonly = 1,
    srcs = ["cgroup_util.cc"],
    hdrs = ["cgroup_util.h"],
    deps = [
        ":cleanup",
        ":fs_util",
        ":mount_util",
        ":posix_error",
        ":temp_path",
        "@com_google_absl//absl/container:flat_hash_map",
        "@com_google_absl//absl/container:flat_hash_set",
        "@com_google_absl//absl/strings",
    ],
)

cc_library(
    name = "socket_util",
    testonly = 1,
    srcs = [
        "socket_util.cc",
        "socket_util_impl.cc",
    ],
    hdrs = ["socket_util.h"],
    defines = select_system(),
    deps = default_net_util() + select_gtest() + [
        ":file_descriptor",
        ":posix_error",
        ":save_util",
        ":temp_path",
        ":test_util",
        ":thread_util",
        "@com_google_absl//absl/memory",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/strings:str_format",
        "@com_google_absl//absl/time",
        "@com_google_absl//absl/types:optional",
    ],
)

cc_library(
    name = "errno_safe_allocator",
    testonly = 1,
    srcs = [
        "errno_safe_allocator.cc",
    ],
)
