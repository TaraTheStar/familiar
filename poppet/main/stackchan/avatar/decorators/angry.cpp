/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "decorators.h"
#include <hal/hal.h>
#include <vector>

using namespace uitk;
using namespace uitk::lvgl_cpp;
using namespace stackchan::avatar;

static const Vector2i _angry_default_position        = Vector2i(108, -70);
static const lv_color_t _angry_default_color         = lv_color_hex(0xFDB034);
static const std::vector<int> _angry_rotation_frames = {150, 200};

LV_IMAGE_DECLARE(decorator_angry);

AngryDecorator::AngryDecorator(lv_obj_t* parent, uint32_t destroyAfterMs, uint32_t animationIntervalMs)
    : _animation_interval_ms(animationIntervalMs)
{
    // Initialize the UI component
    _angry = std::make_unique<Image>(parent);
    _angry->setSrc(&decorator_angry);
    _angry->setAlign(LV_ALIGN_CENTER);
    _angry->setPos(_angry_default_position.x, _angry_default_position.y);

    // Set the rotation pivot and initial angle
    _angry->setTransformPivot(_angry->getWidth() / 2, _angry->getHeight() / 2);
    _angry->setRotation(_angry_rotation_frames[0]);

    // Set the color recolor
    _angry->setImageRecolorOpa(LV_OPA_COVER);
    _angry->setImageRecolor(_angry_default_color);

    uint32_t now = GetHAL().millis();

    // Initialize the destroy countdown
    if (destroyAfterMs > 0) {
        _destroy_at   = now + destroyAfterMs;
        _has_lifetime = true;
    }

    // Initialize the animation countdown
    if (_animation_interval_ms > 0) {
        _next_animation_tick = now + _animation_interval_ms;
    }
}

AngryDecorator::~AngryDecorator()
{
}

void AngryDecorator::_update()
{
    uint32_t now = GetHAL().millis();

    // Check for auto-destroy
    if (_has_lifetime && now >= _destroy_at) {
        requestDestroy();
        return;
    }

    // Check for animation frame change
    if (_animation_interval_ms > 0 && now >= _next_animation_tick) {
        _next_animation_tick = now + _animation_interval_ms;

        // Switch frame
        _animation_index = (_animation_index + 1) % _angry_rotation_frames.size();
        _angry->setRotation(_angry_rotation_frames[_animation_index]);
    }
}

void AngryDecorator::setPosition(int x, int y)
{
    if (_angry) {
        _angry->setPos(x, y);
    }
}

void AngryDecorator::setRotation(int rotation)
{
    if (_angry) {
        _angry->setRotation(rotation);
    }
}

void AngryDecorator::setColor(lv_color_t color)
{
    if (_angry) {
        _angry->setImageRecolor(color);
    }
}
