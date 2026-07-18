/*
 * SPDX-License-Identifier: MIT
 */
#include "familiar_registry.h"
#include "familiar.h"
#include "../default/default.h"

#include <hal/hal.h>                  // LvglLockGuard
#include <hal/board/hal_bridge.h>     // is_xiaozhi_ready / toggle_xiaozhi_chat_state
#include <settings.h>                 // Settings (NVS)
#include <stackchan/stackchan.h>      // GetStackChan

using namespace uitk::lvgl_cpp;

namespace stackchan::avatar {

// NVS namespace/key, sharing the "display" namespace with the LCD theme.
static const char* kNvsNamespace = "display";
static const char* kNvsKey       = "familiar";

std::vector<std::string> knownFamiliars()
{
    return {"default", "cat", "bat", "toad", "fox"};
}

bool isValidFamiliar(const std::string& name)
{
    for (const auto& n : knownFamiliars()) {
        if (n == name) {
            return true;
        }
    }
    return false;
}

// isSpriteFamiliar: a sprite-backed familiar (needs assets), vs the built-in
// procedural "default" face.
static bool isSpriteFamiliar(const std::string& name)
{
    return name != "default" && isValidFamiliar(name);
}

std::string currentFamiliar()
{
    Settings settings(kNvsNamespace);
    std::string value = settings.GetString(kNvsKey, "default");
    if (!isValidFamiliar(value)) {
        value = "default";
    }
    return value;
}

void saveFamiliar(const std::string& name)
{
    Settings settings(kNvsNamespace, /*read_write=*/true);
    settings.SetString(kNvsKey, name);
}

std::unique_ptr<Avatar> buildAvatar(const std::string& name, lv_obj_t* parent)
{
    auto wirePanelTap = [](Container* panel) {
        if (!panel) {
            return;
        }
        panel->onClick().connect([]() {
            if (hal_bridge::is_xiaozhi_ready()) {
                hal_bridge::toggle_xiaozhi_chat_state();
            }
        });
    };

    if (isSpriteFamiliar(name)) {
        auto avatar = std::make_unique<FamiliarAvatar>(name);
        avatar->init(parent);
        wirePanelTap(avatar->getPanel());
        return avatar;
    }

    auto avatar = std::make_unique<DefaultAvatar>();
    avatar->init(parent);
    wirePanelTap(avatar->getPanel());
    return avatar;
}

bool applyFamiliar(const std::string& name)
{
    if (!isValidFamiliar(name)) {
        return false;
    }
    LvglLockGuard lock;  // serialize against the StackChan update task
    auto avatar = buildAvatar(name, lv_screen_active());
    GetStackChan().attachAvatar(std::move(avatar));
    saveFamiliar(name);
    return true;
}

}  // namespace stackchan::avatar
