/*
 * SPDX-License-Identifier: MIT
 *
 * Familiar (avatar skin) registry + selection (PROTOCOL_V2 / WS4 F3).
 *
 * One place to: list the selectable familiars, build the chosen Avatar (the
 * built-in procedural "default" or a sprite familiar like "cat"), persist the
 * choice in NVS, and swap it live. The display's SetupUI builds currentFamiliar()
 * at boot; the "self.avatar.set_familiar" MCP tool calls applyFamiliar() to swap
 * at runtime.
 */
#pragma once
#include "../../avatar/avatar.h"
#include <lvgl.h>
#include <memory>
#include <string>
#include <vector>

namespace stackchan::avatar {

// knownFamiliars lists every selectable name, including "default" (the built-in
// procedural face). Sprite familiars (e.g. "cat") need matching assets shipped.
std::vector<std::string> knownFamiliars();
bool isValidFamiliar(const std::string& name);

// currentFamiliar returns the NVS-stored choice (falls back to "default" if unset
// or invalid). saveFamiliar persists a choice.
std::string currentFamiliar();
void saveFamiliar(const std::string& name);

// buildAvatar constructs + init()s the Avatar for name, wires the panel tap-to-
// toggle-chat handler, and returns it ready to attach. Unknown names fall back to
// the default skin.
std::unique_ptr<Avatar> buildAvatar(const std::string& name, lv_obj_t* parent);

// applyFamiliar swaps the live avatar to name and persists the choice. Takes the
// LVGL lock; safe to call from the MCP tool handler. Returns false for an invalid
// name (no change). The animation modifiers re-target the new avatar each tick.
bool applyFamiliar(const std::string& name);

}  // namespace stackchan::avatar
