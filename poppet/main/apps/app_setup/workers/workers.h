/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include "common.h"
#include <smooth_lvgl.hpp>
#include <uitk/short_namespace.hpp>
#include <hal/hal.h>
#include <cstdint>
#include <memory>
#include <string_view>

namespace setup_workers {

/**
 * @brief
 *
 */
class WorkerBase {
public:
    virtual ~WorkerBase() = default;

    virtual void update()
    {
    }

    bool isDone() const
    {
        return _is_done;
    }

protected:
    bool _is_done = false;
};

/**
 * @brief
 *
 */
class ZeroCalibrationWorker : public WorkerBase {
public:
    ZeroCalibrationWorker();
    void update() override;

private:
    std::unique_ptr<WorkerBase> _page_tips;
    std::unique_ptr<WorkerBase> _page_calibration;
};

/**
 * @brief
 *
 */
class ServoTestWorker : public WorkerBase {
public:
    ServoTestWorker();
    void update() override;

private:
    std::unique_ptr<WorkerBase> _page_tips;
    std::unique_ptr<WorkerBase> _page_test;
    std::unique_ptr<WorkerBase> _page_done;
};

/**
 * @brief
 *
 */
class WifiSetupWorker : public WorkerBase {
public:
    WifiSetupWorker();
    ~WifiSetupWorker();
    void update() override;

private:
    // local-only: hands WiFi setup off to xiaozhi-esp32's built-in captive
    // portal (StartConfigAp + web UI, auto-triggered when no SSID stored).
    // This screen explains the handoff, sets the configured flag, and
    // breaks out of the setup loop so main.cpp can call startXiaozhi().
    bool _is_first_in = true;

    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::unique_ptr<uitk::lvgl_cpp::Label> _title;
    std::unique_ptr<uitk::lvgl_cpp::Label> _info;
    std::unique_ptr<uitk::lvgl_cpp::Button> _btn_continue;
    bool _continue_clicked = false;
};

/**
 * @brief
 *
 */
class RgbTestWorker : public WorkerBase {
public:
    RgbTestWorker();
    ~RgbTestWorker();

private:
    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::vector<std::unique_ptr<uitk::lvgl_cpp::Button>> _buttons;
};

/**
 * @brief
 *
 */
class StartupWorker : public WorkerBase {
public:
    class PageStartup {
    public:
        PageStartup();

        bool isSkipClicked() const
        {
            return _is_skip_clicked;
        }

        bool isStartClicked() const
        {
            return _is_start_clicked;
        }

    private:
        std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
        std::unique_ptr<uitk::lvgl_cpp::Label> _info;
        std::unique_ptr<uitk::lvgl_cpp::Button> _btn_skip;
        std::unique_ptr<uitk::lvgl_cpp::Button> _btn_start;

        bool _is_skip_clicked  = false;
        bool _is_start_clicked = false;
    };

    StartupWorker();
    ~StartupWorker();
    void update() override;

private:
    std::unique_ptr<PageStartup> _page_startup;
    std::unique_ptr<ServoTestWorker> _worker_servo_test;
    std::unique_ptr<WifiSetupWorker> _worker_wifi;
};

/**
 * @brief
 *
 */
class FwVersionWorker : public WorkerBase {
public:
    FwVersionWorker();
    ~FwVersionWorker();
    void update() override;

private:
    uint32_t _last_tick = 0;
};

/**
 * @brief
 *
 */
class SystemUpdateWorker : public WorkerBase {
public:
    SystemUpdateWorker();
    ~SystemUpdateWorker();
    void update() override;
};

/**
 * @brief
 *
 */
class BrightnessSetupWorker : public WorkerBase {
public:
    BrightnessSetupWorker();
    ~BrightnessSetupWorker();
    void update() override;

private:
    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::unique_ptr<uitk::lvgl_cpp::Label> _label_brightness;
    std::unique_ptr<uitk::lvgl_cpp::Slider> _slider;
    std::unique_ptr<uitk::lvgl_cpp::Button> _btn_confirm;
    int32_t _target_brightness = -1;
};

/**
 * @brief
 *
 */
class VolumeSetupWorker : public WorkerBase {
public:
    VolumeSetupWorker();
    ~VolumeSetupWorker();
    void update() override;

private:
    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::unique_ptr<uitk::lvgl_cpp::Label> _label_volume;
    std::unique_ptr<uitk::lvgl_cpp::Slider> _slider;
    std::unique_ptr<uitk::lvgl_cpp::Button> _btn_confirm;
    std::vector<uint8_t> _volume_levels;
    int32_t _target_volume = -1;
};

/**
 * @brief
 *
 */
class TimezoneWorker : public WorkerBase {
public:
    TimezoneWorker();
    ~TimezoneWorker();
    void update() override;

private:
    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::unique_ptr<uitk::lvgl_cpp::Roller> _roller;
    std::unique_ptr<uitk::lvgl_cpp::Button> _btn_confirm;
    std::unique_ptr<uitk::lvgl_cpp::Label> _label;
    bool _confirm_flag = false;
};

/**
 * @brief
 *
 */
class FactoryResetWorker : public WorkerBase {
public:
    FactoryResetWorker();
    ~FactoryResetWorker();
    void update() override;

private:
    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::unique_ptr<uitk::lvgl_cpp::Label> _label_title;
    std::unique_ptr<uitk::lvgl_cpp::Label> _label_info;
    std::unique_ptr<uitk::lvgl_cpp::Button> _btn_cancel;
    std::unique_ptr<uitk::lvgl_cpp::Button> _btn_confirm;

    int _confirm_count = 0;
    bool _cancel_flag  = false;
    bool _confirm_flag = false;

    void update_ui();
};

/**
 * @brief
 *
 */
class AccountWorker : public WorkerBase {
public:
    class PanelInfo {
    public:
        PanelInfo(lv_obj_t* parent, int posY, std::string_view title, std::string_view info);

    private:
        std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
        std::unique_ptr<uitk::lvgl_cpp::Label> _label_title;
        std::unique_ptr<uitk::lvgl_cpp::Label> _label_info;
    };

    class PageAccount {
    public:
        PageAccount(std::string_view username, std::string_view deviceName);

        bool isUnbindClicked() const
        {
            return _is_unbind_clicked;
        }

        bool isQuitClicked() const
        {
            return _is_quit_clicked;
        }

    private:
        std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
        std::unique_ptr<uitk::lvgl_cpp::Label> _label_title;
        std::unique_ptr<PanelInfo> _panel_username;
        std::unique_ptr<PanelInfo> _panel_device_name;
        std::unique_ptr<uitk::lvgl_cpp::Button> _btn_unbind;
        std::unique_ptr<uitk::lvgl_cpp::Button> _btn_quit;

        bool _is_unbind_clicked = false;
        bool _is_quit_clicked   = false;
    };

    AccountWorker();
    ~AccountWorker();
    void update() override;

private:
    std::unique_ptr<PageAccount> _page_account;
    std::unique_ptr<FactoryResetWorker> _worker_reset;
};

}  // namespace setup_workers
