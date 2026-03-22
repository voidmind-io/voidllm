package main

// @title           VoidLLM API
// @version         0.2.0
// @description     Lightweight LLM proxy with org/team/user/key hierarchy, usage tracking, and model management.
// @BasePath        /api/v1

// @securityDefinitions.apikey BearerAuth
// @in              header
// @name            Authorization
// @description     API key (vl_uk_, vl_tk_, vl_sa_) or session key (vl_sk_)

// @tag.name        auth
// @tag.description Authentication and user profile
// @tag.name        keys
// @tag.description API key management
// @tag.name        orgs
// @tag.description Organization management
// @tag.name        teams
// @tag.description Team management
// @tag.name        users
// @tag.description User management
// @tag.name        org-members
// @tag.description Organization membership management
// @tag.name        team-members
// @tag.description Team membership management
// @tag.name        service-accounts
// @tag.description Service account management
// @tag.name        models
// @tag.description Model registry management
// @tag.name        model-access
// @tag.description Model access control (allowlists)
// @tag.name        model-aliases
// @tag.description Model alias management
// @tag.name        invites
// @tag.description User invitation management
// @tag.name        dashboard
// @tag.description Dashboard statistics
// @tag.name        usage
// @tag.description Usage analytics
