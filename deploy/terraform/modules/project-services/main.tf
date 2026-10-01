resource "google_project_service" "this" {
  for_each = toset(var.services)

  project = var.project_id
  service = each.value

  # Shared project: never disable a service other consumers of the project may
  # depend on when this stack is destroyed. The project may host other
  # workloads beyond this module set, so a `terraform destroy` here must not
  # turn off APIs (or dependent APIs) that those other consumers rely on.
  disable_on_destroy         = false
  disable_dependent_services = false
}

data "google_project" "this" {
  project_id = var.project_id

  depends_on = [google_project_service.this]
}
