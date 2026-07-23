/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "servo.h"
#include <hal/hal.h>
#include "esp_log.h"

using namespace uitk;

namespace stackchan::motion {

static const char* TAG_SERVO = "motion";  // shares grep tag with Motion-level log

static SpringOptions_t _default_spring_options = {
    .stiffness = 170.0,
    .damping   = 26.0,

    .mass     = 1.0,
    .velocity = 0.0,

    .restSpeed = 0.1,
    .restDelta = 0.1,

    .duration       = 0.0,
    .bounce         = 0.0,
    .visualDuration = 0.0,
};

void Servo::init()
{
    apply_default_spring_options();

    _angle_anim.teleport(getCurrentAngle());
    update();

    setTorqueEnabled(false);
}

// Settle window after the last motion command before auto-releasing torque.
// The spring animation reporting done() means the command stream has reached
// the target; this gives the servo a moment to physically settle before we cut
// torque, replacing the old per-tick is_moving_impl() serial poll.
static constexpr uint32_t kTorqueReleaseSettleMs = 250;

void Servo::update()
{
    // Keep update in at most 50Hz
    if (GetHAL().millis() - _last_tick < 20) {
        return;
    }
    _last_tick = GetHAL().millis();

    // Apply animation
    if (!_angle_anim.done()) {
        _angle_anim.updateWithDelta(0.02f);  // Fixed delta time for consistency
        set_angle_impl(static_cast<int>(_angle_anim.directValue()));
        // A position write powers/holds the servo; track that locally so the
        // rest branch knows torque is on without asking the servo over serial.
        _torque_enabled = true;
        _last_motion_ms = _last_tick;
        return;
    }

    // Snap to target angle when animation ends
    if (_snap_to_target_on_rest) {
        _snap_to_target_on_rest = false;
        set_angle_impl(_angle_anim.end);
        _torque_enabled = true;
        _last_motion_ms = _last_tick;
        return;
    }

    // Auto release torque on rest.
    //
    // 2c: previously this branch called isMoving() (→ is_moving_impl() = a
    // blocking ReadMove serial round-trip) every tick and getTorqueEnabled()
    // (→ ReadToqueEnable) every 200 ms — i.e. ~50 Hz × 2 servos of UART
    // traffic while the head sat perfectly still. That idle bus contention is
    // what starved the audio tasks. We now drive release purely from local
    // state: the spring animation being done() means commands have reached the
    // target, so after a fixed settle window we drop torque exactly once using
    // the cached flag. No serial traffic is issued while resting.
    if (_auto_torque_release_enabled && _torque_enabled) {
        if (_last_tick - _last_motion_ms > kTorqueReleaseSettleMs) {
            setTorqueEnabled(false);  // updates _torque_enabled → one-shot
        }
    }
}

void Servo::move(int angle)
{
    apply_default_spring_options();
    update_angle_anim_target(angle);
}

void Servo::moveWithSpringParams(int angle, float stiffness, float damping)
{
    _angle_anim.springOptions().visualDuration = 0.0f;  // Disable timing override
    _angle_anim.springOptions().stiffness      = stiffness;
    _angle_anim.springOptions().damping        = damping;

    update_angle_anim_target(angle);
}

void Servo::moveWithSpeed(int angle, int speed, const char* tag)
{
    ESP_LOGI(TAG_SERVO, "HEADMOVE [%s] servo angle=%d speed=%d t=%lu",
             tag, angle, speed, (unsigned long)GetHAL().millis());
    auto spring_options = map_speed_to_spring_options(speed);
    moveWithSpringParams(angle, spring_options.stiffness, spring_options.damping);
}

int Servo::getCurrentAngle()
{
    return _angle_anim.directValue();
}

bool Servo::isMoving()
{
    return _angle_anim.done() == false || is_moving_impl();
}

void Servo::apply_default_spring_options()
{
    auto& options          = _angle_anim.springOptions();
    options.visualDuration = 0.0f;  // Disable timing override
    options.stiffness      = _default_spring_options.stiffness;
    options.damping        = _default_spring_options.damping;
}

void Servo::update_angle_anim_target(int angle)
{
    angle = uitk::clamp(angle, _angle_limit.x, _angle_limit.y);

    if (_auto_angle_sync_enabled) {
        _angle_anim.teleport(getCurrentAngle());  // Use current angle as start
    }
    _angle_anim             = angle;  // Apply new target
    _snap_to_target_on_rest = true;
}

uitk::SpringOptions_t Servo::map_speed_to_spring_options(int speed)
{
    speed = uitk::clamp(speed, 0, 1000);

    // 1. Compute stiffness
    // Use a quadratic mapping: k = k_min + (speed/1000)^2 * k_range
    // At speed=500, k is roughly 10 + 0.25 * 640 = 170
    float k_min           = 10.0f;
    float k_max           = 650.0f;
    float normalizedSpeed = speed / 1000.0f;
    float stiffness       = k_min + (normalizedSpeed * normalizedSpeed) * (k_max - k_min);

    // 2. Compute damping
    // To keep critical damping (no overshoot, fastest settling), the formula is d = 2 * sqrt(m * k)
    // For a slight bounce, lower the coefficient from 2.0 to 1.5~1.8
    float mass    = 1.0f;
    float damping = 2.0f * sqrtf(mass * stiffness);

    // 3. Build the options
    uitk::SpringOptions_t options = _default_spring_options;
    options.stiffness             = stiffness;
    options.damping               = damping;
    options.mass                  = mass;

    // 4. Dynamically adjust the rest thresholds
    // A larger threshold at high speed prevents tiny jitter caused by discrete computation
    if (speed > 800) {
        options.restDelta = 0.5f;
        options.restSpeed = 0.5f;
    } else {
        options.restDelta = 0.1f;
        options.restSpeed = 0.1f;
    }

    return options;
}

}  // namespace stackchan::motion
