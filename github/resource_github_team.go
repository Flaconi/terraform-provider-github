package github

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/go-github/v41/github"
	"github.com/hashicorp/terraform-plugin-sdk/helper/customdiff"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/shurcooL/githubv4"
)

/*
These constants are used to retry API on various operations.
This is required because Terraform apply/destroy runs in parallel and when
looping through a module or resource a team name could have been changed by another thread,
a parent team could have been removed or various other parallel issues.
To mitigate this, we're simply retrying the API to double check its actual state.
See their corresponding for loops for further description.
*/
const github_team_api_retry = 10
const github_team_api_wait = 5

func resourceGithubTeam() *schema.Resource {
	return &schema.Resource{
		Create: resourceGithubTeamCreate,
		Read:   resourceGithubTeamRead,
		Update: resourceGithubTeamUpdate,
		Delete: resourceGithubTeamDelete,
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		CustomizeDiff: customdiff.Sequence(
			customdiff.ComputedIf("slug", func(d *schema.ResourceDiff, meta interface{}) bool {
				return d.HasChange("name")
			}),
		),

		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				Required: true,
			},
			"description": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"privacy": {
				Type:         schema.TypeString,
				Optional:     true,
				Default:      "secret",
				ValidateFunc: validateValueFunc([]string{"secret", "closed"}),
			},
			"parent_team_id": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "ID or slug of parent team",
			},
			"ldap_dn": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"create_default_maintainer": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},
			"slug": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"etag": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"node_id": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"members_count": {
				Type:     schema.TypeInt,
				Computed: true,
			},
		},
	}
}

func resourceGithubTeamCreate(d *schema.ResourceData, meta interface{}) error {
	err := checkOrganization(meta)
	if err != nil {
		return err
	}

	client := meta.(*Owner).v3client

	ownerName := meta.(*Owner).name
	name := d.Get("name").(string)

	newTeam := github.NewTeam{
		Name:        name,
		Description: github.String(d.Get("description").(string)),
		Privacy:     github.String(d.Get("privacy").(string)),
	}

	if parentTeamIdString, ok := d.GetOk("parent_team_id"); ok {
		/*
			When creating nested teams via Terraform by looping through a module or resource
			the parent team might not have been created yet (in "terraform apply" parallel runs),
			so we are giving it some time to create the parent team and will repeatedly check
			if the parent exists (has been created by another parallel run).
		*/
		teamId, err := getTeamID(parentTeamIdString.(string), meta)
		for i := 0; i < github_team_api_retry; i++ {
			// Try again on error
			if err != nil {
				log.Printf("[WARN] Fetching parent team: Retry (%d/%d)", i, github_team_api_retry)
				time.Sleep(github_team_api_wait * time.Second)
				teamId, err = getTeamID(parentTeamIdString.(string), meta)
				continue
			}
			// Exit loop on success
			break
		}
		if err != nil {
			log.Printf("[ERROR] Unable to find parent team")
			return err
		}
		newTeam.ParentTeamID = &teamId
	}
	ctx := context.Background()

	log.Printf("[DEBUG] Creating team: %s (%s)", name, ownerName)
	githubTeam, _, err := client.Teams.CreateTeam(ctx,
		ownerName, newTeam)
	if err != nil {
		return err
	}

	create_default_maintainer := d.Get("create_default_maintainer").(bool)
	if !create_default_maintainer {
		log.Printf("[DEBUG] Removing default maintainer from team: %s (%s)", name, ownerName)
		if err := removeDefaultMaintainer(*githubTeam.Slug, meta); err != nil {
			return err
		}
	}

	if ldapDN := d.Get("ldap_dn").(string); ldapDN != "" {
		mapping := &github.TeamLDAPMapping{
			LDAPDN: github.String(ldapDN),
		}
		_, _, err = client.Admin.UpdateTeamLDAPMapping(ctx, githubTeam.GetID(), mapping)
		if err != nil {
			return err
		}
	}

	d.SetId(strconv.FormatInt(githubTeam.GetID(), 10))
	return resourceGithubTeamRead(d, meta)
}

func resourceGithubTeamRead(d *schema.ResourceData, meta interface{}) error {
	err := checkOrganization(meta)
	if err != nil {
		return err
	}

	client := meta.(*Owner).v3client
	orgId := meta.(*Owner).id

	id, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return unconvertibleIdErr(d.Id(), err)
	}
	ctx := context.WithValue(context.Background(), ctxId, d.Id())
	if !d.IsNewResource() {
		ctx = context.WithValue(ctx, ctxEtag, d.Get("etag").(string))
	}

	/*
		Slug-name specific (as opposed to using team ID):
		When using slug-name to read GitHub teams it could be that another parallel thread of TF
		(when looping through a module or resource) still needs to apply changes (rename the team name)
		and thus it could be that we don't find it right away.
		In order to mitigate this, we will loop this call and give the API a sane waiting time, hoping
		the other thread has finished renaming the team in the mean time.
	*/
	log.Printf("[DEBUG] Reading team: %s", d.Id())
	team, resp, err := client.Teams.GetTeamByID(ctx, orgId, id)
	for i := 0; i < github_team_api_retry; i++ {
		if err != nil {
			if ghErr, ok := err.(*github.ErrorResponse); ok {
				if ghErr.Response.StatusCode == http.StatusNotModified {
					return nil
				}
				// When using slug-name instead of ID, the new team name might not have been changed
				// so we need to include this in the loop.
				if ghErr.Response.StatusCode == http.StatusNotFound {
					log.Printf("[WARN] Looking up team: Retry on 404 (%d/%d)", i, github_team_api_retry)
					time.Sleep(github_team_api_wait * time.Second)
					team, resp, err = client.Teams.GetTeamByID(ctx, orgId, id)
					continue
				}
				log.Printf("[WARN] Looking up team: Retry on error (%d/%d)", i, github_team_api_retry)
				time.Sleep(github_team_api_wait * time.Second)
				team, resp, err = client.Teams.GetTeamByID(ctx, orgId, id)
				continue
			}
			return err
		}
		// Exit loop on success
		break
	}
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok {
			if ghErr.Response.StatusCode == http.StatusNotModified {
				return nil
			}
			if ghErr.Response.StatusCode == http.StatusNotFound {
				log.Printf("[WARN] Removing team %s from state because it no longer exists in GitHub",
					d.Id())
				d.SetId("")
				return nil
			}
		}
		return err
	}

	d.Set("etag", resp.Header.Get("ETag"))
	d.Set("description", team.GetDescription())
	d.Set("name", team.GetName())
	d.Set("privacy", team.GetPrivacy())
	if parent := team.Parent; parent != nil {
		d.Set("parent_team_id", strconv.FormatInt(parent.GetID(), 10))
	} else {
		d.Set("parent_team_id", "")
	}
	d.Set("ldap_dn", team.GetLDAPDN())
	d.Set("slug", team.GetSlug())
	d.Set("node_id", team.GetNodeID())
	d.Set("members_count", team.GetMembersCount())

	return nil
}

func resourceGithubTeamUpdate(d *schema.ResourceData, meta interface{}) error {
	err := checkOrganization(meta)
	if err != nil {
		return err
	}

	client := meta.(*Owner).v3client
	orgId := meta.(*Owner).id

	editedTeam := github.NewTeam{
		Name:        d.Get("name").(string),
		Description: github.String(d.Get("description").(string)),
		Privacy:     github.String(d.Get("privacy").(string)),
	}

	if parentTeamIdString, ok := d.GetOk("parent_team_id"); ok {
		/*
			Slug-name specific (as opposed to using team ID):
			When updating nested teams via Terraform by looping through a module or resource
			the parent team might not have been updated by a new slug-name yet
			(in "terraform apply" parallel runs), so we are giving it some time to create the parent
			team and will repeatedly check if the parent exists
			(has been created by another parallel run).
		*/
		teamId, err := getTeamID(parentTeamIdString.(string), meta)
		for i := 0; i < github_team_api_retry; i++ {
			// Try again on error
			if err != nil {
				log.Printf("[WARN] Fetching parent team: Retry (%d/%d)", i, github_team_api_retry)
				time.Sleep(github_team_api_wait * time.Second)
				teamId, err = getTeamID(parentTeamIdString.(string), meta)
				continue
			}
			// Exit loop on success
			break
		}
		if err != nil {
			log.Printf("[ERROR] Unable to find parent team")
			return err
		}
		editedTeam.ParentTeamID = &teamId
	}

	teamId, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return unconvertibleIdErr(d.Id(), err)
	}
	ctx := context.WithValue(context.Background(), ctxId, d.Id())

	log.Printf("[DEBUG] Updating team: %s", d.Id())
	team, _, err := client.Teams.EditTeamByID(ctx, orgId, teamId, editedTeam, false)
	if err != nil {
		return err
	}

	if d.HasChange("ldap_dn") {
		ldapDN := d.Get("ldap_dn").(string)
		mapping := &github.TeamLDAPMapping{
			LDAPDN: github.String(ldapDN),
		}
		_, _, err = client.Admin.UpdateTeamLDAPMapping(ctx, team.GetID(), mapping)
		if err != nil {
			return err
		}
	}

	d.SetId(strconv.FormatInt(team.GetID(), 10))
	return resourceGithubTeamRead(d, meta)
}

func resourceGithubTeamDelete(d *schema.ResourceData, meta interface{}) error {
	err := checkOrganization(meta)
	if err != nil {
		return err
	}

	client := meta.(*Owner).v3client
	orgId := meta.(*Owner).id

	id, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return unconvertibleIdErr(d.Id(), err)
	}
	ctx := context.WithValue(context.Background(), ctxId, d.Id())

	log.Printf("[DEBUG] Deleting team: %s", d.Id())
	_, err = client.Teams.DeleteTeamByID(ctx, orgId, id)
	/*
		When deleting a team and it failed, we need to check if it has already been deleted meanwhile.
		This could be the case when deleting nested teams via Terraform by looping through a module
		or resource and the parent team might have been deleted already. If the parent team had
		been deleted already (via parallel runs), the child team is also already gone (deleted by
		GitHub automatically).
		So we're checking if it still exists and if not, simply remove it from TF state.
	*/
	if err != nil {
		_, _, err = client.Teams.GetTeamByID(ctx, orgId, id)
		if err != nil {
			if ghErr, ok := err.(*github.ErrorResponse); ok {
				if ghErr.Response.StatusCode == http.StatusNotFound {
					log.Printf("[WARN] Removing team: %s from state because it no longer exists",
						d.Id())
					d.SetId("")
					return nil
				}
			}
			// In case of a different error, return it as well
			log.Printf("[ERROR] Failed to delete team: %s", d.Id())
			return err
		}
	}
	return err
}

func removeDefaultMaintainer(teamSlug string, meta interface{}) error {

	client := meta.(*Owner).v3client
	orgName := meta.(*Owner).name
	v4client := meta.(*Owner).v4client

	type User struct {
		Login githubv4.String
	}

	var query struct {
		Organization struct {
			Team struct {
				Members struct {
					Nodes []User
				}
			} `graphql:"team(slug:$slug)"`
		} `graphql:"organization(login:$login)"`
	}
	variables := map[string]interface{}{
		"slug":  githubv4.String(teamSlug),
		"login": githubv4.String(orgName),
	}

	err := v4client.Query(meta.(*Owner).StopContext, &query, variables)
	if err != nil {
		return err
	}

	for _, user := range query.Organization.Team.Members.Nodes {
		log.Printf("[DEBUG] Removing default maintainer from team: %s", user.Login)
		_, err := client.Teams.RemoveTeamMembershipBySlug(meta.(*Owner).StopContext, orgName, teamSlug, string(user.Login))
		if err != nil {
			return err
		}
	}

	return nil
}
