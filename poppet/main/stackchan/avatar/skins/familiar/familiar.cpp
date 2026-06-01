/*
 * SPDX-License-Identifier: MIT
 */
#include "familiar.h"
#include <assets/assets.h>

using namespace uitk;
using namespace uitk::lvgl_cpp;
using namespace stackchan::avatar;

// Base placements (offsets from panel center, matching the default skin's
// geometry). Eyes mirror on x; gaze offsets are small nudges the idle/look
// modifiers drive through setPosition.
static const Vector2i _eye_base   = Vector2i(64, -20);
static const int _eye_gaze        = 16;
static const Vector2i _mouth_base = Vector2i(0, 44);
static const int _mouth_gaze      = 12;

// Frame-select thresholds on the Feature weight the modifiers drive.
static const int _eye_open_at   = 50;  // BlinkModifier drops to 25 -> closed
static const int _mouth_open_at = 40;  // SpeakingModifier rides 40..80 -> open

/* ------------------------------- SpriteEyes ------------------------------- */

SpriteEyes::SpriteEyes(lv_obj_t* parent, const std::string& prefix, bool isLeftEye)
{
    _is_left_eye = isLeftEye;
    _open_dsc    = assets::get_image(prefix + "_eye_open.png");
    _closed_dsc  = assets::get_image(prefix + "_eye_closed.png");

    _img = std::make_unique<Image>(parent);
    _img->setAlign(LV_ALIGN_CENTER);
    _img->setSrc(&_open_dsc);
    _open = true;

    setWeight(100);
    setPosition(_position);
}

SpriteEyes::~SpriteEyes()
{
    _img.reset();
}

void SpriteEyes::applyFrame()
{
    bool open = _weight >= _eye_open_at;
    if (open == _open) {
        return;
    }
    _open = open;
    _img->setSrc(open ? &_open_dsc : &_closed_dsc);
}

void SpriteEyes::setPosition(const Vector2i& position)
{
    Element::setPosition(position);

    int base_x = _is_left_eye ? -_eye_base.x : _eye_base.x;
    int gaze_x = map_range(_position.x, -100, 100, -_eye_gaze, _eye_gaze);
    int gaze_y = map_range(_position.y, -100, 100, -_eye_gaze, _eye_gaze);
    _img->setPos(base_x + gaze_x, _eye_base.y + gaze_y);
}

void SpriteEyes::setWeight(int weight)
{
    Feature::setWeight(weight);
    applyFrame();
}

void SpriteEyes::setRotation(int rotation)
{
    // Sprite eyes stay upright in this PoC (no per-emotion tilt); the open/closed
    // frame + the costume carry the expression. Rotation art is a later refinement.
    Element::setRotation(rotation);
}

void SpriteEyes::setEmotion(const Emotion& emotion)
{
    if (getIgnoreEmotion()) {
        return;
    }
    // Map emotion -> eye openness (mirrors the default skin's weights). Below the
    // open threshold the closed frame shows, giving sleepy/sad their droop.
    int weight = 100;
    switch (emotion) {
        case Emotion::Sad:
            weight = 40;
            break;
        case Emotion::Sleepy:
            weight = 35;
            break;
        case Emotion::Love:
            weight = 60;
            break;
        case Emotion::Angry:
            weight = 70;
            break;
        case Emotion::Happy:
            weight = 72;
            break;
        case Emotion::Doubt:
            weight = 75;
            break;
        case Emotion::Surprise:
        case Emotion::Neutral:
        default:
            weight = 100;
            break;
    }
    setWeight(weight);
}

void SpriteEyes::setVisible(bool visible)
{
    Element::setVisible(visible);
    _img->setHidden(!visible);
}

/* ------------------------------- SpriteMouth ------------------------------ */

SpriteMouth::SpriteMouth(lv_obj_t* parent, const std::string& prefix)
{
    _closed_dsc = assets::get_image(prefix + "_mouth_closed.png");
    _open_dsc   = assets::get_image(prefix + "_mouth_open.png");

    _img = std::make_unique<Image>(parent);
    _img->setAlign(LV_ALIGN_CENTER);
    _img->setSrc(&_closed_dsc);
    _open = false;

    setWeight(0);
    setPosition(_position);
}

SpriteMouth::~SpriteMouth()
{
    _img.reset();
}

void SpriteMouth::applyFrame()
{
    bool open = _weight >= _mouth_open_at;
    if (open == _open) {
        return;
    }
    _open = open;
    _img->setSrc(open ? &_open_dsc : &_closed_dsc);
}

void SpriteMouth::setPosition(const Vector2i& position)
{
    Element::setPosition(position);

    int gaze_x = map_range(_position.x, -100, 100, -_mouth_gaze, _mouth_gaze);
    int gaze_y = map_range(_position.y, -100, 100, -_mouth_gaze, _mouth_gaze);
    _img->setPos(_mouth_base.x + gaze_x, _mouth_base.y + gaze_y);
}

void SpriteMouth::setWeight(int weight)
{
    Feature::setWeight(weight);
    applyFrame();
}

void SpriteMouth::setRotation(int rotation)
{
    Element::setRotation(rotation);  // sprite mouth stays upright (PoC)
}

void SpriteMouth::setVisible(bool visible)
{
    Element::setVisible(visible);
    _img->setHidden(!visible);
}

/* ------------------------------ FamiliarAvatar ---------------------------- */

void FamiliarAvatar::init(lv_obj_t* parent, const lv_font_t* font)
{
    _panel = std::make_unique<Container>(parent);
    _panel->align(LV_ALIGN_CENTER, 0, 0);
    _panel->setSize(320, 240);
    _panel->setRadius(0);
    _panel->setBorderWidth(0);
    _panel->setBgColor(lv_color_black());
    _panel->removeFlag(LV_OBJ_FLAG_SCROLLABLE);
    _panel->setPadding(0, 0, 0, 0);

    // Costume first so it sits BEHIND the eyes/mouth (z-order = creation order).
    _face_dsc = assets::get_image(_name + "_face.png");
    _costume  = std::make_unique<Image>(_panel->get());
    _costume->setAlign(LV_ALIGN_CENTER);
    _costume->setSrc(&_face_dsc);

    _key_elements.leftEye      = std::make_unique<SpriteEyes>(_panel->get(), _name, true);
    _key_elements.rightEye     = std::make_unique<SpriteEyes>(_panel->get(), _name, false);
    _key_elements.mouth        = std::make_unique<SpriteMouth>(_panel->get(), _name);
    _key_elements.speechBubble =
        std::make_unique<DefaultSpeechBubble>(_panel->get(), lv_color_white(), lv_color_black(), font);
}

Container* FamiliarAvatar::getPanel() const
{
    return _panel ? _panel.get() : nullptr;
}
