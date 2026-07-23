/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include "../modifiable.h"
#include "../utils/random.h"
#include <smooth_ui_toolkit.hpp>
#include <hal/hal.h>
#include <cstdint>

namespace stackchan {

/**
 * @brief
 *
 */
class IdleExpressionModifier : public Modifier {
public:
    IdleExpressionModifier(uint32_t interval_min = 2000, uint32_t interval_max = 6000)
        : _interval_min(interval_min), _interval_max(interval_max)
    {
        _next_tick = GetHAL().millis() + 500;
    }

    void _update(Modifiable& stackchan) override
    {
        if (!stackchan.hasAvatar() || stackchan.avatar().isModifyLocked()) {
            return;
        }

        uint32_t now = GetHAL().millis();
        if (now < _next_tick) {
            return;
        }

        // Perform a random micro-expression
        perform_idle_emotion(stackchan.avatar());

        // Calculate the next trigger time
        uint32_t delay = Random::getInstance().getInt(_interval_min, _interval_max);
        _next_tick     = now + delay;
    }

private:
    void perform_idle_emotion(avatar::Avatar& avatar)
    {
        int action = Random::getInstance().getInt(0, 100);

        if (action < 70) {
            // [Action 1: Wandering gaze] Move both eyes together within a small range
            int offsetX = Random::getInstance().getInt(-20, 20);
            int offsetY = Random::getInstance().getInt(-15, 15);
            avatar.leftEye().setPosition({offsetX, offsetY});
            avatar.rightEye().setPosition({offsetX, offsetY});

            // Move the mouth a little to match
            avatar.mouth().setPosition({0, Random::getInstance().getInt(0, 10)});
        } else if (action < 80) {
            // [Action 3: Tilt the mouth] Rotation angle
            // Rotation: 0~3600
            int rotation = Random::getInstance().getInt(-30, 30);
            // Add the base value
            avatar.mouth().setRotation(rotation < 0 ? 3600 + rotation : rotation);
        } else {
            // [Action 4: Return to neutral]
            reset_to_neutral(avatar);
        }
    }

    void reset_to_neutral(avatar::Avatar& avatar)
    {
        // Return position to center
        avatar.leftEye().setPosition({0, 0});
        avatar.rightEye().setPosition({0, 0});
        avatar.mouth().setPosition({0, 0});

        // Return scale to normal
        avatar.leftEye().setSize(0);
        avatar.rightEye().setSize(0);

        // Return rotation and weight
        avatar.mouth().setRotation(0);
        avatar.mouth().setWeight(0);
    }

    uint32_t _interval_min;
    uint32_t _interval_max;
    uint32_t _next_tick = 0;
};

}  // namespace stackchan
