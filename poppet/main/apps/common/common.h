/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include "home_indicator/home_indicator.h"
#include "loading_page/loading_page.h"
#include "status_bar/status_bar.h"
#include "reminder/reminder.h"
#include "toast/toast.h"
#include <string_view>

namespace common {

// Product + version string shown on the firmware/about screen and boot screen.
// FIRMWARE_VERSION is the git tag, injected from CMake (see CMakeLists.txt).
inline constexpr std::string_view FirmwareVersion = "stackchan - familiar (poppet) " FIRMWARE_VERSION;

}
