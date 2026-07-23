#ifndef PRESS_TO_TALK_MCP_TOOL_H
#define PRESS_TO_TALK_MCP_TOOL_H

#include "mcp_server.h"
#include "settings.h"

// Reusable MCP tool class for the press-to-talk mode
class PressToTalkMcpTool {
private:
    bool press_to_talk_enabled_;

public:
    PressToTalkMcpTool();
    
    // Initialize the tool and register it with the MCP server
    void Initialize();

    // Get the current press-to-talk mode state
    bool IsPressToTalkEnabled() const;

private:
    // Callback function for the MCP tool
    ReturnValue HandleSetPressToTalk(const PropertyList& properties);

    // Internal method: set the press-to-talk state and save it to settings
    void SetPressToTalkEnabled(bool enabled);
};

#endif // PRESS_TO_TALK_MCP_TOOL_H 