-- DM participants are peers, so every one of them must hold the channel-admin
-- role.
--
-- CreateDM inserted every participant with role 'member', while Update and
-- Archive both require 'admin'. The consequence was that no DM or group DM
-- could ever be renamed or archived by anybody — group DMs in particular were
-- stuck with name = NULL forever. The handler now creates DM members as
-- admins; this backfills the conversations that already exist.

UPDATE channel_members cm
   SET role = 'admin'
  FROM channels ch
 WHERE ch.id = cm.channel_id
   AND ch.type IN ('dm', 'group_dm')
   AND cm.role <> 'admin';
