-- Reverses 011_dm_participant_roles.up.sql.
--
-- Before 011 every DM participant was created with role 'member' and no code
-- path ever promoted one, so demoting them all is an exact inverse for rows
-- that predate the change. Rolling back restores the original defect: DMs and
-- group DMs become un-renameable and un-archivable again.

UPDATE channel_members cm
   SET role = 'member'
  FROM channels ch
 WHERE ch.id = cm.channel_id
   AND ch.type IN ('dm', 'group_dm')
   AND cm.role <> 'member';
