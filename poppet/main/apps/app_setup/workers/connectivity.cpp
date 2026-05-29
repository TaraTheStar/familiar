/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "workers.h"
#include <src/misc/lv_area.h>
#include <src/misc/lv_text.h>
#include <stackchan/stackchan.h>
#include <mooncake_log.h>
#include <hal/hal.h>
#include <memory>

using namespace smooth_ui_toolkit::lvgl_cpp;
using namespace setup_workers;
using namespace stackchan;

static std::string _tag = "Setup-Connectivity";

// local-only: this worker no longer drives WiFi setup itself. The original
// flow (App Store / Play Store QR codes -> BLE pairing with the M5Stack
// mobile app -> app pushes WiFi creds over BLE) has been retired in
// favour of xiaozhi-esp32's built-in hotspot/captive-portal flow, which
// auto-triggers when no SSID is stored (see wifi_board.cc TryWifiConnect).
// This screen is now a one-tap handoff: explain what's about to happen,
// mark the device "configured" so main.cpp skips the setup app on next
// boot, and request a xiaozhi start so the captive portal kicks in.
//
// The BLE config server (Hal::startAppConfigServer) and WifiConfigServer
// in hal_ble.cpp are intentionally left in place but no longer reached
// from the first-boot path. Removing them is a separate cleanup pass.
WifiSetupWorker::WifiSetupWorker()
{
    auto avatar = std::make_unique<avatar::DefaultAvatar>();
    avatar->init(lv_screen_active(), &lv_font_montserrat_24);
    avatar->leftEye().setVisible(false);
    avatar->rightEye().setVisible(false);
    avatar->mouth().setVisible(false);
    GetStackChan().attachAvatar(std::move(avatar));
}

WifiSetupWorker::~WifiSetupWorker()
{
    GetStackChan().resetAvatar();
}

void WifiSetupWorker::update()
{
    if (_is_first_in) {
        _is_first_in = false;

        _panel = std::make_unique<Container>(lv_screen_active());
        _panel->setBgColor(lv_color_hex(0xEDF4FF));
        _panel->align(LV_ALIGN_CENTER, 0, 0);
        _panel->setBorderWidth(0);
        _panel->setSize(320, 240);
        _panel->setRadius(0);

        _title = std::make_unique<Label>(lv_screen_active());
        _title->setTextFont(&lv_font_montserrat_20);
        _title->setTextColor(lv_color_hex(0x7E7B9C));
        _title->align(LV_ALIGN_TOP_MID, 0, 0);
        _title->setText("WIFI SETUP");

        _info = std::make_unique<Label>(lv_screen_active());
        _info->setTextFont(&lv_font_montserrat_14);
        _info->setTextColor(lv_color_hex(0x26206A));
        _info->align(LV_ALIGN_TOP_MID, 0, 36);
        _info->setTextAlign(LV_TEXT_ALIGN_CENTER);
        _info->setText(
            "Your device will host a WiFi\n"
            "network named \"Xiaozhi-...\".\n\n"
            "Join it from your phone or laptop\n"
            "and follow the prompts to set\n"
            "your home WiFi credentials.");

        _btn_continue = std::make_unique<Button>(lv_screen_active());
        apply_button_common_style(*_btn_continue);
        _btn_continue->align(LV_ALIGN_BOTTOM_MID, 0, -20);
        _btn_continue->setSize(180, 42);
        _btn_continue->label().setText("Continue");
        _btn_continue->onClick().connect([this]() { _continue_clicked = true; });
    }

    if (_continue_clicked) {
        mclog::tagInfo(_tag, "handoff to xiaozhi captive portal");
        GetHAL().setAppConfiged();
        GetHAL().requestXiaozhiStart();
        _is_done = true;
    }
}
