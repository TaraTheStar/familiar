# The following five lines of boilerplate have to be in your project's
# CMakeLists in this exact order for cmake to work correctly
cmake_minimum_required(VERSION 3.16)

# Derive the firmware version from the current git tag so the boot and about
# screens always reflect the release (e.g. v2.0.0, or v2.0.0-3-gdeadbee-dirty on
# a work tree). Resolved at configure time; reconfigure to pick up a new tag.
# Falls back to a pinned string when built outside a git checkout.
find_package(Git QUIET)
set(PROJECT_VER "v2.0.0")
if(GIT_FOUND)
    execute_process(
        COMMAND ${GIT_EXECUTABLE} describe --tags --always --dirty
        WORKING_DIRECTORY ${CMAKE_CURRENT_LIST_DIR}
        OUTPUT_VARIABLE _git_describe
        OUTPUT_STRIP_TRAILING_WHITESPACE
        ERROR_QUIET
    )
    if(_git_describe)
        set(PROJECT_VER "${_git_describe}")
    endif()
endif()

add_definitions(-DFIRMWARE_VERSION=\"${PROJECT_VER}\")

# Add this line to disable the specific warning
add_compile_options(-Wno-missing-field-initializers)

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(stack-chan)
