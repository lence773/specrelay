CREATE INDEX events_visible_id_idx
  ON events(id)
  WHERE event_type <> 'agent.output';

CREATE INDEX events_visible_project_id_idx
  ON events(project_id, id)
  WHERE event_type <> 'agent.output';
